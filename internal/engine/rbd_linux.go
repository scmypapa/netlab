//go:build linux

package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

var cephName = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func cephCommand(ctx context.Context, tool, root, user string, args ...string) ([]byte, error) {
	args = append([]string{"--conf", filepath.Join(root, "ceph.conf"), "--id", user}, args...)
	return commandOutput(ctx, tool, args...)
}

func (d vmDisk) rbdCommand(ctx context.Context, args ...string) ([]byte, error) {
	return cephCommand(ctx, "rbd", d.root, d.rbd.User, append([]string{"--pool", d.rbd.Pool}, args...)...)
}

func rbdStorageInfo(ctx context.Context, root string, storage api.RbdStorage) (api.StorageInfo, error) {
	out, err := cephCommand(ctx, "ceph", root, storage.User, "df", "--format", "json")
	if err != nil {
		return api.StorageInfo{}, err
	}
	var df struct {
		Pools []struct {
			Name  string `json:"name"`
			Stats struct {
				Available int64 `json:"max_avail"`
				Used      int64 `json:"bytes_used"`
			} `json:"stats"`
		} `json:"pools"`
	}
	if err = json.Unmarshal(out, &df); err != nil {
		return api.StorageInfo{}, err
	}
	for _, pool := range df.Pools {
		if pool.Name == storage.Pool {
			return api.StorageInfo{Path: root, Filesystem: storage.Fsid + "/" + storage.Pool, CapacityBytes: pool.Stats.Available + pool.Stats.Used, AvailableBytes: pool.Stats.Available, NativeSnapshots: true, Rbd: &storage}, nil
		}
	}
	return api.StorageInfo{}, fmt.Errorf("Ceph 存储池 %s 不存在", storage.Pool)
}

func (e *Engine) registerRBD(ctx context.Context, id, root string, input api.CephConnection) (api.StorageInfo, error) {
	if !cephName.MatchString(input.Pool) || !cephName.MatchString(input.User) || len(input.Monitors) == 0 {
		return api.StorageInfo{}, errors.New("请填写 Ceph 存储池、客户端和 MON 地址")
	}
	for _, endpoint := range input.Monitors {
		if _, _, err := net.SplitHostPort(endpoint); err != nil {
			return api.StorageInfo{}, fmt.Errorf("MON 地址 %s 应为 host:port", endpoint)
		}
	}
	if input.Key == nil {
		return api.StorageInfo{}, errors.New("请填写 Ceph 客户端密钥")
	}
	key, err := base64.StdEncoding.DecodeString(*input.Key)
	if err != nil || len(key) == 0 {
		return api.StorageInfo{}, errors.New("Ceph 客户端密钥格式无效")
	}
	keyring := fmt.Sprintf("[client.%s]\nkey = %s\n", input.User, *input.Key)
	if err = os.WriteFile(filepath.Join(root, "ceph.keyring"), []byte(keyring), 0600); err != nil {
		return api.StorageInfo{}, err
	}
	config := fmt.Sprintf("[global]\nmon_host = %s\n[client.%s]\nkeyring = %s\n", strings.Join(input.Monitors, ","), input.User, filepath.Join(root, "ceph.keyring"))
	if err = os.WriteFile(filepath.Join(root, "ceph.conf"), []byte(config), 0600); err != nil {
		return api.StorageInfo{}, err
	}
	out, err := cephCommand(ctx, "ceph", root, input.User, "fsid")
	if err != nil {
		return api.StorageInfo{}, err
	}
	storage := api.RbdStorage{Pool: input.Pool, User: input.User, Monitors: input.Monitors, Fsid: strings.TrimSpace(string(out)), ImagePrefix: id + ".", SecretId: id}
	info, err := rbdStorageInfo(ctx, root, storage)
	if err != nil {
		return info, err
	}
	if err = writeRecoveryJSON(filepath.Join(root, "storage.json"), info); err != nil {
		return info, err
	}
	definition := libvirtxml.Secret{UUID: id, Private: "yes", Ephemeral: "no", Usage: &libvirtxml.SecretUsage{Type: "ceph", Name: "netlab/" + id}}
	xml, err := definition.Marshal()
	if err != nil {
		return info, err
	}
	secret, err := e.vm.conn.SecretDefineXML(xml, 0)
	if err != nil {
		return info, err
	}
	defer secret.Free()
	if err = secret.SetValue(key, 0); err != nil {
		return info, err
	}
	return info, nil
}

func (e *Engine) removeRBD(ctx context.Context, info api.StorageInfo) error {
	disk := vmDisk{rbd: info.Rbd, root: info.Path}
	names, err := disk.rbdImages(ctx)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return errors.New("存储池仍包含虚拟磁盘或恢复点")
	}
	secret, err := e.vm.conn.LookupSecretByUUIDString(info.Rbd.SecretId)
	var native libvirt.Error
	if errors.As(err, &native) && native.Code == libvirt.ERR_NO_SECRET {
		return nil
	}
	if err != nil {
		return err
	}
	defer secret.Free()
	return secret.Undefine()
}

func (d vmDisk) rbdImages(ctx context.Context) ([]string, error) {
	out, err := d.rbdCommand(ctx, "ls", "--format", "json")
	var names []string
	if err == nil {
		err = json.Unmarshal(out, &names)
	}
	return slices.DeleteFunc(names, func(name string) bool { return !strings.HasPrefix(name, d.rbd.ImagePrefix) }), err
}

func (d vmDisk) exists(ctx context.Context) (bool, error) {
	if d.rbd == nil {
		_, err := os.Stat(d.file)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	names, err := d.rbdImages(ctx)
	return slices.Contains(names, d.image), err
}

func (d vmDisk) prepare(ctx context.Context, source string, sizeGiB int64) (err error) {
	exists, err := d.exists(ctx)
	if err != nil {
		return err
	}
	if exists {
		if sizeGiB == 0 {
			return nil
		}
		return expandDisk(ctx, d.address(), sizeGiB)
	}
	if d.rbd == nil {
		if err = os.MkdirAll(filepath.Dir(d.file), 0711); err != nil {
			return err
		}
		if source == "" {
			return command(ctx, "qemu-img", "create", "-f", "qcow2", d.file, fmt.Sprintf("%dG", sizeGiB))
		}
		if err = command(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", source, d.file); err != nil {
			return err
		}
		return expandDisk(ctx, d.file, sizeGiB)
	}
	staging := d
	staging.image += ".pending"
	if err = staging.remove(ctx); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, staging.remove(context.WithoutCancel(ctx)))
		}
	}()
	sizeMiB := sizeGiB << 10
	if source != "" {
		image, inspectErr := inspectImage(ctx, source)
		if inspectErr != nil {
			return inspectErr
		}
		sizeMiB = max(sizeMiB, (image.VirtualSize+(1<<20)-1)>>20)
	}
	_, err = staging.rbdCommand(ctx, "create", staging.image, "--size", fmt.Sprint(sizeMiB))
	if err == nil && source != "" {
		err = command(ctx, "qemu-img", "convert", "-n", "-f", "qcow2", "-O", "raw", source, staging.address())
		if err == nil && sizeGiB > 0 {
			err = expandDisk(ctx, staging.address(), sizeGiB)
		}
	}
	if err != nil {
		return err
	}
	_, err = d.rbdCommand(ctx, "rename", staging.image, d.image)
	return err
}

func (d vmDisk) remove(ctx context.Context) error {
	if d.rbd == nil {
		return os.RemoveAll(d.file)
	}
	exists, err := d.exists(ctx)
	if err != nil || !exists {
		return err
	}
	snapshots, err := d.snapshots(ctx)
	if err != nil || len(snapshots) > 0 {
		return err
	}
	_, err = d.rbdCommand(ctx, "rm", d.image)
	return err
}

type rbdSnapshot struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (d vmDisk) usedBytes(ctx context.Context) (int64, error) {
	name := d.image
	if d.snapshot != "" {
		name += "@" + d.snapshot
	}
	out, err := d.rbdCommand(ctx, "du", name, "--format", "json")
	if err != nil {
		return 0, err
	}
	var usage struct {
		Images []struct {
			Used int64 `json:"used_size"`
		} `json:"images"`
	}
	if err = json.Unmarshal(out, &usage); err != nil {
		return 0, err
	}
	var bytes int64
	for _, image := range usage.Images {
		bytes += image.Used
	}
	return bytes, nil
}

func (d vmDisk) snapshots(ctx context.Context) ([]rbdSnapshot, error) {
	out, err := d.rbdCommand(ctx, "snap", "ls", d.image, "--format", "json")
	var snapshots []rbdSnapshot
	if err == nil {
		err = json.Unmarshal(out, &snapshots)
	}
	return snapshots, err
}

func (d vmDisk) capture(ctx context.Context, point string) error {
	snapshots, err := d.snapshots(ctx)
	if err != nil {
		return err
	}
	for _, snap := range snapshots {
		if snap.Name == point {
			return nil
		}
	}
	_, err = d.rbdCommand(ctx, "snap", "create", d.image+"@"+point)
	return err
}

func (d vmDisk) restoreFile(ctx context.Context, source string) error {
	if d.rbd == nil {
		if err := os.MkdirAll(filepath.Dir(d.file), 0711); err != nil {
			return err
		}
		return os.Rename(source, d.file)
	}
	if err := d.remove(ctx); err != nil {
		return err
	}
	return d.prepare(ctx, source, 0)
}

func (d vmDisk) restoreSnapshot(ctx context.Context, source vmDisk) (err error) {
	staging := d
	staging.image += ".pending"
	if err = staging.remove(ctx); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, staging.remove(context.WithoutCancel(ctx)))
		}
	}()
	src := source.rbd.Pool + "/" + source.image + "@" + source.snapshot
	target := d.rbd.Pool + "/" + staging.image
	if _, err = d.rbdCommand(ctx, "--rbd-default-clone-format", "2", "clone", src, target); err != nil {
		return err
	}
	if _, err = staging.rbdCommand(ctx, "flatten", staging.image); err != nil {
		return err
	}
	_, err = d.rbdCommand(ctx, "rename", staging.image, d.image)
	return err
}

func (v *VirtualMachines) deleteDiskSnapshots(ctx context.Context, env, point string, a api.AssetExecution) error {
	live := map[string]bool{}
	for _, volume := range a.VolumeSources {
		live[persistentDisk(volume).key()] = true
	}
	domain, err := v.conn.LookupDomainByUUIDString(a.InstanceId)
	if err == nil {
		defer domain.Free()
		actual, readErr := v.observedExecution(ctx, domain)
		if readErr != nil {
			return readErr
		}
		for _, disk := range executionDisks(v.data, env, *actual) {
			live[disk.key()] = true
		}
	} else if !noDomain(err) {
		return err
	}
	for _, disk := range executionDisks(v.data, env, a) {
		if disk.rbd == nil {
			continue
		}
		exists, err := disk.exists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		snapshots, err := disk.snapshots(ctx)
		if err != nil {
			return err
		}
		for _, snap := range snapshots {
			if snap.Name == point {
				if _, err = disk.rbdCommand(ctx, "snap", "rm", disk.image+"@"+point); err != nil {
					return err
				}
			}
		}
		if !live[disk.key()] {
			if err = disk.remove(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func executionDisks(data, env string, a api.AssetExecution) []vmDisk {
	directory := assetDirectory(data, env, a)
	disks := []vmDisk{}
	if a.Template.Disks != nil {
		for i := range *a.Template.Disks {
			disks = append(disks, systemDisk(directory, a, i))
		}
	}
	if a.Asset.Volumes != nil {
		for _, volume := range *a.Asset.Volumes {
			disks = append(disks, volumeDisk(directory, env, a, volume.Id))
		}
	}
	return disks
}

func executionDiskMap(data, env string, a api.AssetExecution) map[string]vmDisk {
	directory := assetDirectory(data, env, a)
	disks := map[string]vmDisk{}
	if a.Template.Disks != nil {
		for i, disk := range *a.Template.Disks {
			disks["image-"+disk.Id] = systemDisk(directory, a, i)
		}
	}
	if a.Asset.Volumes != nil {
		for _, volume := range *a.Asset.Volumes {
			disks["volume-"+volume.Id] = volumeDisk(directory, env, a, volume.Id)
		}
	}
	return disks
}
