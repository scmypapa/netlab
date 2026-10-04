//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func (v *VirtualMachines) captureTemplate(ctx context.Context, t api.Template, source api.TemplateCaptureSource, staging, directory string) (api.Template, error) {
	d, err := v.conn.LookupDomainByUUIDString(source.InstanceId)
	if err != nil {
		return t, err
	}
	defer d.Free()
	owner, err := v.owned(d, source.EnvironmentId, source.AssetId)
	if err != nil {
		return t, err
	}
	state, err := vmState(d)
	if err != nil {
		return t, err
	}
	if state != "stopped" {
		return t, errors.New("请先关闭虚拟机，再固化模板")
	}
	var installed api.AssetExecution
	if err = json.Unmarshal([]byte(owner.Execution), &installed); err != nil {
		return t, err
	}
	text, err := d.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return t, err
	}
	var config libvirtxml.Domain
	if err = config.Unmarshal(text); err != nil {
		return t, err
	}
	t.Kind, t.Os, t.Hardware = api.Vm, installed.Template.Os, installed.Template.Hardware
	t.NicModels = installed.Template.NicModels
	t.Resources = installed.Asset.Resources
	t.Resources.DiskGiB = 0
	t.Format, t.Media, t.Volumes, t.StateFiles = ptr(api.Qcow2), nil, nil, nil
	definitions := []api.TemplateDisk{}
	for _, disk := range config.Devices.Disks {
		if disk.Device != "disk" {
			continue
		}
		location, err := diskFromDomain(disk, installed)
		if err != nil {
			return t, err
		}
		id := strings.TrimPrefix(strings.TrimPrefix(disk.Serial, "image-"), "volume-")
		definition := api.TemplateDisk{Id: id, Bus: api.TemplateDiskBus(disk.Target.Bus), BootOrder: len(definitions) + 1}
		for _, original := range *installed.Template.Disks {
			if original.Id == id {
				definition = original
			}
		}
		definition.BootOrder = len(definitions) + 1
		if disk.Address != nil && disk.Address.Drive != nil {
			drive := disk.Address.Drive
			if drive.Controller != nil {
				definition.ControllerIndex = ptr(int(*drive.Controller))
				for _, controller := range config.Devices.Controllers {
					if controller.Type == string(definition.Bus) && controller.Index != nil && *controller.Index == *drive.Controller && controller.Model != "" {
						definition.ControllerModel = &controller.Model
					}
				}
			}
			if drive.Unit != nil {
				unit := int(*drive.Unit)
				if definition.Bus == api.Ide && drive.Bus != nil {
					unit += int(*drive.Bus) * 2
				}
				definition.ControllerUnit = &unit
			}
		}
		image, err := inspectImage(ctx, location.address())
		if err != nil {
			return t, err
		}
		definition.SizeGiB = (image.VirtualSize + (1 << 30) - 1) / (1 << 30)
		t.Resources.DiskGiB += definition.SizeGiB
		if err = command(ctx, "qemu-img", "convert", "-f", image.Format, "-O", "qcow2", location.address(), systemDiskPath(staging, len(definitions))); err != nil {
			return t, fmt.Errorf("disk %s: %w", id, err)
		}
		definitions = append(definitions, definition)
	}
	t.Disks = &definitions
	files := []string{}
	if config.OS.NVRam != nil {
		if err = copyArtifact(ctx, config.OS.NVRam.NVRam, filepath.Join(staging, "nvram.fd")); err != nil {
			return t, err
		}
		files = append(files, "nvram.fd")
	}
	if len(config.Devices.TPMs) > 0 {
		if err = archiveTPM(tpmDirectory(source.InstanceId), filepath.Join(staging, "tpm.tar")); err != nil {
			return t, err
		}
		files = append(files, "tpm.tar")
	}
	if len(files) > 0 {
		t.StateFiles = &files
	}
	return commitVMTemplate(t, staging, directory)
}
