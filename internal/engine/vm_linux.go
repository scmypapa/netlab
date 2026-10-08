//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

type VirtualMachines struct {
	conn         *libvirt.Connect
	data, bridge string
	sources      sync.Map
	hardware     api.VmHardware
}

func NewVirtualMachines(uri, data, bridge string) (*VirtualMachines, error) {
	if err := libvirtEvents(); err != nil {
		return nil, err
	}
	c, err := libvirt.NewConnect(uri)
	if err != nil {
		return nil, err
	}
	v := &VirtualMachines{conn: c, data: data, bridge: bridge}
	if v.hardware, err = v.hardwareCapabilities(); err != nil {
		c.Close()
		return nil, err
	}
	return v, nil
}
func (v *VirtualMachines) Close() { v.conn.Close() }
func noDomain(err error) bool {
	var e libvirt.Error
	return errors.As(err, &e) && e.Code == libvirt.ERR_NO_DOMAIN
}
func (v *VirtualMachines) Execute(ctx context.Context, env string, phase api.NodePlanPhase, a api.AssetExecution) (string, error) {
	if phase == api.NodePlanPhasePrepare {
		return v.prepare(ctx, env, a)
	}
	d, err := v.conn.LookupDomainByUUIDString(a.InstanceId)
	if noDomain(err) {
		if phase == api.NodePlanPhaseDestroy {
			return "destroyed", v.removeFiles(ctx, env, a)
		}
		return "absent", err
	}
	if err != nil {
		return "unknown", err
	}
	defer d.Free()
	owner, err := v.owned(d, env, a.Asset.Id)
	if err != nil {
		return "unknown", err
	}
	state, err := vmState(d)
	if err != nil {
		return "unknown", err
	}
	switch phase {
	case api.NodePlanPhaseCleanupVolumes:
		references, err := v.volumeReferences(env, a.Asset.Id)
		if err != nil {
			return state, err
		}
		return state, v.removeVolumes(ctx, env, a, references, true)
	case api.NodePlanPhaseUpdate:
		return v.update(ctx, d, env, a)
	case api.NodePlanPhaseStart:
		if state == "stopped" {
			err = d.Create()
		}
		if err == nil {
			err = v.bindGuestNetwork(ctx, d, a)
			if err != nil {
				current, observeErr := vmState(d)
				return current, errors.Join(err, observeErr)
			}
		}
	case api.NodePlanPhaseStop:
		if state == "suspended" {
			if err = d.Resume(); err != nil {
				return state, err
			}
			state = "running"
		}
		if state != "stopped" {
			err = d.Shutdown()
			if err == nil {
				return waitVM(ctx, d, "stopped")
			}
		}
	case api.NodePlanPhaseForceStop:
		if state != "stopped" {
			err = d.Destroy()
		}
	case api.NodePlanPhaseSuspend:
		if state != "suspended" {
			err = d.Suspend()
		}
	case api.NodePlanPhaseResume:
		if state == "suspended" {
			err = d.Resume()
		}
	case api.NodePlanPhaseDestroy:
		if err = json.Unmarshal([]byte(owner.Execution), &a); err != nil {
			return state, err
		}
		if state != "stopped" {
			if err = d.Destroy(); err != nil {
				return state, err
			}
		}
		flags := libvirt.DOMAIN_UNDEFINE_MANAGED_SAVE | libvirt.DOMAIN_UNDEFINE_SNAPSHOTS_METADATA | libvirt.DOMAIN_UNDEFINE_NVRAM | libvirt.DOMAIN_UNDEFINE_TPM
		if err = d.UndefineFlags(flags); err != nil {
			return "stopped", err
		}
		return "destroyed", v.removeFiles(ctx, env, a)
	case api.NodePlanPhaseInspect:
	default:
		return state, fmt.Errorf("invalid virtual machine phase %s", phase)
	}
	if err != nil {
		return state, err
	}
	return vmState(d)
}
func (v *VirtualMachines) prepare(ctx context.Context, env string, a api.AssetExecution) (string, error) {
	if d, err := v.conn.LookupDomainByUUIDString(a.InstanceId); err == nil {
		defer d.Free()
		if _, err = v.owned(d, env, a.Asset.Id); err != nil {
			return "unknown", err
		}
		return vmState(d)
	} else if !noDomain(err) {
		return "unknown", err
	}
	dir := assetDirectory(v.data, env, a)
	if err := os.MkdirAll(dir, 0711); err != nil {
		return "absent", err
	}
	template := a.Template
	var err error
	templateDir := templateRuntimeDirectory(v.data, template.Id, template.Version)
	if a.Rbd == nil {
		template, err = v.prepareTemplate(ctx, template, nil)
		if err != nil {
			return "absent", err
		}
		templateDir = templateDirectory(v.data, template.Id, template.Version)
	}
	a.Template = template
	if err = restoreTemplateState(ctx, templateDir, dir, a.InstanceId, template); err != nil {
		return "absent", err
	}
	if template.Media != nil {
		for index := range *template.Media {
			target := templateMediaPath(dir, index)
			source := templateMediaPath(templateDir, index)
			if err = os.Symlink(source, target); err != nil && !errors.Is(err, os.ErrExist) {
				return "absent", err
			}
		}
	}
	sizes, err := systemDiskSizes(a)
	if err != nil {
		return "absent", err
	}
	for index, size := range sizes {
		disk := systemDisk(dir, a, index)
		if a.Rbd != nil {
			base := sharedTemplateDisk(*a.StoragePath, *a.Rbd, template, index)
			err = disk.cloneSnapshot(ctx, base, size)
		} else {
			err = disk.prepare(ctx, systemDiskPath(templateDir, index), size)
		}
		if err != nil {
			return "absent", err
		}
	}
	if a.Asset.Volumes != nil {
		if err = v.prepareVolumes(ctx, env, a); err != nil {
			return "absent", err
		}
	}
	media, err := stageInitialization(ctx, dir, a)
	if err != nil {
		return "absent", err
	}
	if media != "" {
		defer os.RemoveAll(filepath.Dir(media))
	}
	config, err := DomainXML(env, dir, v.bridge, a)
	if err != nil {
		return "absent", err
	}
	d, err := v.conn.DomainDefineXML(config)
	if err != nil {
		return "absent", err
	}
	defer d.Free()
	if media != "" {
		if err = os.Rename(media, filepath.Join(dir, "initialization.iso")); err != nil {
			return "stopped", err
		}
	}
	return vmState(d)
}
func (v *VirtualMachines) removeFiles(ctx context.Context, env string, a api.AssetExecution) error {
	if err := os.RemoveAll(tpmDirectory(a.InstanceId)); err != nil {
		return err
	}
	directory := assetDirectory(v.data, env, a)
	if a.Rbd != nil && a.Template.Disks != nil {
		for index := range *a.Template.Disks {
			if err := systemDisk(directory, a, index).remove(ctx); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	if a.Asset.Volumes == nil || len(*a.Asset.Volumes) == 0 {
		return nil
	}
	references, err := v.volumeReferences(env, a.Asset.Id)
	if err != nil {
		return err
	}
	return v.removeVolumes(ctx, env, a, references, false)
}

func (v *VirtualMachines) removeVolumes(ctx context.Context, env string, a api.AssetExecution, references map[string]bool, requireUnused bool) error {
	directory := assetDirectory(v.data, env, a)
	return removeVolumeFiles(a.Asset.Volumes, references, func(id string) string { return volumeDisk(directory, env, a, id).key() }, func(id string) error { return volumeDisk(directory, env, a, id).remove(ctx) }, requireUnused)
}

func (v *VirtualMachines) prepareVolumes(ctx context.Context, env string, a api.AssetExecution) error {
	for _, volume := range *a.Asset.Volumes {
		if _, persistent := a.VolumeSources[volume.Id]; persistent {
			exists, err := volumeDisk(assetDirectory(v.data, env, a), env, a, volume.Id).exists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("persistent volume %s is missing", volume.Id)
			}
			continue
		}
		if err := volumeDisk(assetDirectory(v.data, env, a), env, a, volume.Id).prepare(ctx, "", volume.SizeGiB); err != nil {
			return err
		}
	}
	return nil
}

func (v *VirtualMachines) volumeReferences(env, asset string) (map[string]bool, error) {
	references := make(map[string]bool)
	domains, err := v.conn.ListAllDomains(0)
	if err != nil {
		return nil, err
	}
	defer func() {
		for i := range domains {
			domains[i].Free()
		}
	}()
	for _, domain := range domains {
		text, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
		if noDomain(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var config libvirtxml.Domain
		if err = config.Unmarshal(text); err != nil {
			return nil, err
		}
		var owner Ownership
		if config.Metadata == nil {
			continue
		}
		if err = xml.Unmarshal([]byte(config.Metadata.XML), &owner); err != nil {
			continue
		}
		if owner.Environment == env && owner.Asset == asset {
			var a api.AssetExecution
			if err = json.Unmarshal([]byte(owner.Execution), &a); err != nil {
				return nil, err
			}
			for _, disk := range config.Devices.Disks {
				if disk.Device != "disk" {
					continue
				}
				location, err := diskFromDomain(disk, a)
				if err != nil {
					return nil, err
				}
				references[location.key()] = true
			}
		}
	}
	return references, nil
}
func (v *VirtualMachines) owned(d *libvirt.Domain, env, asset string) (Ownership, error) {
	text, err := d.GetXMLDesc(0)
	if err != nil {
		return Ownership{}, err
	}
	var config libvirtxml.Domain
	if err = config.Unmarshal(text); err != nil {
		return Ownership{}, err
	}
	var owner Ownership
	if config.Metadata == nil {
		return owner, errors.New("domain is not managed by Netlab")
	}
	if err = xml.Unmarshal([]byte(config.Metadata.XML), &owner); err != nil {
		return owner, err
	}
	if env != "" && owner.Environment != env || asset != "" && owner.Asset != asset {
		return owner, errors.New("domain ownership does not match plan")
	}
	return owner, nil
}
func (v *VirtualMachines) Inventory(ctx context.Context, env string) ([]api.ExecutionResult, error) {
	domains, err := v.conn.ListAllDomains(0)
	if err != nil {
		return nil, err
	}
	results := []api.ExecutionResult{}
	for _, d := range domains {
		xmlDesc, err := d.GetXMLDesc(0)
		if err != nil {
			d.Free()
			if noDomain(err) {
				continue
			}
			return results, err
		}
		var config libvirtxml.Domain
		if err = config.Unmarshal(xmlDesc); err != nil {
			d.Free()
			return results, err
		}
		if config.Metadata == nil {
			d.Free()
			continue
		}
		var owner Ownership
		if err = xml.Unmarshal([]byte(config.Metadata.XML), &owner); err != nil || owner.XMLName.Space != "urn:netlab:instance" {
			d.Free()
			continue
		}
		if env == "" || owner.Environment == env {
			state, err := vmState(&d)
			r := api.ExecutionResult{EnvironmentId: ptr(owner.Environment), AssetId: owner.Asset, InstanceId: owner.Instance, State: state, ObservedAt: time.Now().UTC()}
			var observeErr error
			r.Execution, observeErr = v.observedExecution(ctx, &d)
			err = errors.Join(err, observeErr)
			if err != nil {
				r.Error = ptr(err.Error())
			}
			results = append(results, r)
		}
		d.Free()
	}
	return results, nil
}
func vmState(d *libvirt.Domain) (string, error) {
	s, _, err := d.GetState()
	if err != nil {
		return "unknown", err
	}
	switch s {
	case libvirt.DOMAIN_RUNNING, libvirt.DOMAIN_BLOCKED:
		return "running", nil
	case libvirt.DOMAIN_PAUSED, libvirt.DOMAIN_PMSUSPENDED:
		return "suspended", nil
	case libvirt.DOMAIN_SHUTOFF:
		return "stopped", nil
	case libvirt.DOMAIN_SHUTDOWN:
		return "stopping", nil
	case libvirt.DOMAIN_CRASHED:
		return "crashed", nil
	default:
		return "unknown", nil
	}
}
func waitVM(ctx context.Context, d *libvirt.Domain, target string) (string, error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()
	for {
		state, err := vmState(d)
		if err != nil || state == target {
			return state, err
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-timeout.C:
			return state, fmt.Errorf("guest has not completed normal shutdown")
		case <-ticker.C:
		}
	}
}
func command(ctx context.Context, name string, args ...string) error {
	_, err := commandOutput(ctx, name, args...)
	return err
}

func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%s: %w: %s", name, err, cephKeyDiagnostic.ReplaceAll(exit.Stderr, []byte("type=key val=[redacted]")))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return output, nil
}

type diskImage struct {
	Format          string `json:"format"`
	VirtualSize     int64  `json:"virtual-size"`
	BackingFilename string `json:"backing-filename"`
}

func inspectImage(ctx context.Context, path string) (diskImage, error) {
	var image diskImage
	output, err := commandOutput(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return image, err
	}
	if err = json.Unmarshal(output, &image); err != nil {
		return image, err
	}
	return image, nil
}

func expandDisk(ctx context.Context, path string, sizeGiB int64) error {
	image, err := inspectImage(ctx, path)
	if err != nil {
		return err
	}
	size := sizeGiB * (1 << 30)
	if size < image.VirtualSize {
		return fmt.Errorf("disk %s cannot be shrunk without discarding guest data", filepath.Base(path))
	}
	if size > image.VirtualSize {
		return command(ctx, "qemu-img", "resize", path, fmt.Sprint(size))
	}
	return nil
}
