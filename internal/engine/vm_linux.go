//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
}

func NewVirtualMachines(uri, data, bridge string) (*VirtualMachines, error) {
	c, err := libvirt.NewConnect(uri)
	if err != nil {
		return nil, err
	}
	return &VirtualMachines{conn: c, data: data, bridge: bridge}, nil
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
			return "destroyed", v.removeFiles(env, a)
		}
		return "absent", err
	}
	if err != nil {
		return "unknown", err
	}
	defer d.Free()
	if _, err = v.owned(d, env, a.Asset.Id); err != nil {
		return "unknown", err
	}
	state, err := vmState(d)
	if err != nil {
		return "unknown", err
	}
	switch phase {
	case api.NodePlanPhaseActivate, api.NodePlanPhaseStart:
		if state == "stopped" {
			err = d.Create()
		}
	case api.NodePlanPhaseStop:
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
	case api.NodePlanPhaseReboot:
		err = d.Reboot(0)
	case api.NodePlanPhaseSuspend:
		if state != "suspended" {
			err = d.Suspend()
		}
	case api.NodePlanPhaseResume:
		if state == "suspended" {
			err = d.Resume()
		}
	case api.NodePlanPhaseDestroy:
		if state != "stopped" {
			if err = d.Destroy(); err != nil {
				return state, err
			}
		}
		flags := libvirt.DOMAIN_UNDEFINE_MANAGED_SAVE | libvirt.DOMAIN_UNDEFINE_SNAPSHOTS_METADATA | libvirt.DOMAIN_UNDEFINE_NVRAM | libvirt.DOMAIN_UNDEFINE_TPM
		if err = d.UndefineFlags(flags); err != nil {
			return "stopped", err
		}
		return "destroyed", v.removeFiles(env, a)
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
	dir := instanceDir(v.data, env, a.InstanceId)
	if err := os.MkdirAll(dir, 0711); err != nil {
		return "absent", err
	}
	source, err := v.source(ctx, a.Template)
	if err != nil {
		return "absent", err
	}
	image, err := inspectImage(ctx, source)
	if err != nil {
		return "absent", err
	}
	format := image.Format
	disk := filepath.Join(dir, "system.qcow2")
	if _, err = os.Stat(disk); errors.Is(err, os.ErrNotExist) {
		if err = command(ctx, "qemu-img", "create", "-f", "qcow2", "-F", format, "-b", source, disk); err != nil {
			return "absent", err
		}
	}
	// Resize only upwards; guest partition expansion is a separate observed action.
	if err = command(ctx, "qemu-img", "resize", disk, fmt.Sprintf("%dG", a.Asset.Resources.DiskGiB)); err != nil {
		return "absent", err
	}
	if a.Asset.Volumes != nil {
		for _, vol := range *a.Asset.Volumes {
			path := filepath.Join(v.data, "environments", env, "volumes", a.Asset.Id, vol.Id+".qcow2")
			if err = os.MkdirAll(filepath.Dir(path), 0711); err != nil {
				return "absent", err
			}
			if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err = command(ctx, "qemu-img", "create", "-f", "qcow2", path, fmt.Sprintf("%dG", vol.SizeGiB)); err != nil {
					return "absent", err
				}
			}
		}
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
	return vmState(d)
}
func (v *VirtualMachines) source(ctx context.Context, t api.Template) (string, error) {
	u, err := url.Parse(t.Source)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Scheme == "file" {
		path := t.Source
		if u.Scheme == "file" {
			path = u.Path
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return "", err
		}
		if _, err = os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid VM artifact source scheme")
	}
	lock, _ := v.sources.LoadOrStore(t.Id, &sync.Mutex{})
	m := lock.(*sync.Mutex)
	m.Lock()
	defer m.Unlock()
	path := filepath.Join(v.data, "artifacts", t.Id, fmt.Sprint(t.Version), "image")
	if _, err = os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0711); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Source, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("artifact download: HTTP %d", resp.StatusCode)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	if err = os.Chmod(f.Name(), 0640); err != nil {
		return "", err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
func (v *VirtualMachines) removeFiles(env string, a api.AssetExecution) error {
	if err := os.RemoveAll(instanceDir(v.data, env, a.InstanceId)); err != nil {
		return err
	}
	if a.Asset.Volumes != nil {
		for _, vol := range *a.Asset.Volumes {
			if vol.Retain == nil || !*vol.Retain {
				path := filepath.Join(v.data, "environments", env, "volumes", a.Asset.Id, vol.Id+".qcow2")
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	return nil
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
func (v *VirtualMachines) Inventory(env string) ([]api.ExecutionResult, error) {
	domains, err := v.conn.ListAllDomains(0)
	if err != nil {
		return nil, err
	}
	results := []api.ExecutionResult{}
	for _, d := range domains {
		xmlDesc, err := d.GetXMLDesc(0)
		if err != nil {
			d.Free()
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
			r := api.ExecutionResult{AssetId: owner.Asset, InstanceId: owner.Instance, State: state, ObservedAt: time.Now().UTC()}
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
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, output)
	}
	return nil
}

type diskImage struct {
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtual-size"`
}

func inspectImage(ctx context.Context, path string) (diskImage, error) {
	var image diskImage
	output, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", path).Output()
	if err != nil {
		return image, fmt.Errorf("qemu image inspection: %w", err)
	}
	if err = json.Unmarshal(output, &image); err != nil {
		return image, err
	}
	return image, nil
}
