package engine

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

// A managed disk has one native source, used by creation, inspection and recovery.
type vmDisk struct {
	file     string
	image    string
	snapshot string
	rbd      *api.RbdStorage
	root     string
}

func systemDisk(directory string, a api.AssetExecution, index int) vmDisk {
	disk := vmDisk{file: systemDiskPath(directory, index)}
	if a.Rbd != nil {
		disk = vmDisk{image: a.Rbd.ImagePrefix + a.InstanceId + "." + a.DataSetId + fmt.Sprintf(".disk-%d", index), rbd: a.Rbd, root: *a.StoragePath}
	}
	return disk
}

func volumeDisk(directory, env string, a api.AssetExecution, id string) vmDisk {
	if source, ok := a.VolumeSources[id]; ok {
		return persistentDisk(source)
	}
	disk := vmDisk{file: filepath.Join(filepath.Dir(filepath.Dir(directory)), "volumes", a.Asset.Id, a.DataSetId, id+".qcow2")}
	if a.Rbd != nil {
		disk = vmDisk{image: a.Rbd.ImagePrefix + env + "." + a.Asset.Id + "." + a.DataSetId + ".volume-" + id, rbd: a.Rbd, root: *a.StoragePath}
	}
	return disk
}

func persistentDisk(volume api.NodeVolume) vmDisk {
	disk := vmDisk{file: filepath.Join(volume.Storage.Path, "volumes", volume.Id+".qcow2")}
	if volume.Storage.Rbd != nil {
		disk = vmDisk{image: volume.Storage.Rbd.ImagePrefix + "volume-" + volume.Id, rbd: volume.Storage.Rbd, root: volume.Storage.Path}
	}
	return disk
}

func storageRoot(data string, a api.AssetExecution) string {
	if a.StoragePath != nil {
		return *a.StoragePath
	}
	return data
}

func (d vmDisk) format() string {
	if d.rbd != nil {
		return "raw"
	}
	return "qcow2"
}

func (d vmDisk) key() string {
	if d.rbd != nil {
		return "rbd:" + d.rbd.Fsid + "/" + d.rbd.Pool + "/" + d.image
	}
	return d.file
}

func (d vmDisk) address() string {
	if d.rbd == nil {
		return d.file
	}
	options := map[string]any{"driver": "rbd", "pool": d.rbd.Pool, "image": d.image, "user": d.rbd.User, "conf": filepath.Join(d.root, "ceph.conf")}
	if d.snapshot != "" {
		options["snapshot"] = d.snapshot
	}
	raw, _ := json.Marshal(map[string]any{"driver": "raw", "file": options})
	return "json:" + string(raw)
}

func (d vmDisk) source() (*libvirtxml.DomainDiskSource, *libvirtxml.DomainDiskAuth, error) {
	if d.rbd == nil {
		return &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: d.file}}, nil, nil
	}
	hosts := make([]libvirtxml.DomainDiskSourceHost, 0, len(d.rbd.Monitors))
	for _, endpoint := range d.rbd.Monitors {
		host, port, err := net.SplitHostPort(endpoint)
		if err != nil {
			return nil, nil, err
		}
		hosts = append(hosts, libvirtxml.DomainDiskSourceHost{Name: host, Port: port})
	}
	source := &libvirtxml.DomainDiskSource{Network: &libvirtxml.DomainDiskSourceNetwork{Protocol: "rbd", Name: d.rbd.Pool + "/" + d.image, Hosts: hosts}}
	auth := &libvirtxml.DomainDiskAuth{Username: d.rbd.User, Secret: &libvirtxml.DomainDiskSecret{Type: "ceph", UUID: d.rbd.SecretId}}
	return source, auth, nil
}

func diskFromDomain(disk libvirtxml.DomainDisk, a api.AssetExecution) (vmDisk, error) {
	if disk.Source != nil && disk.Source.File != nil {
		return vmDisk{file: disk.Source.File.File}, nil
	}
	if disk.Source != nil && disk.Source.Network != nil && a.Rbd != nil {
		image, matches := strings.CutPrefix(disk.Source.Network.Name, a.Rbd.Pool+"/")
		if disk.Source.Network.Protocol == "rbd" && matches && strings.HasPrefix(image, a.Rbd.ImagePrefix) {
			return vmDisk{image: image, rbd: a.Rbd, root: *a.StoragePath}, nil
		}
	}
	return vmDisk{}, fmt.Errorf("disk %s has an unmanaged source", disk.Serial)
}
