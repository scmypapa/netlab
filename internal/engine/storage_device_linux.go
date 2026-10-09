//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"netlab.local/core/api"
)

type blockDevice struct {
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Size        int64         `json:"size"`
	Model       string        `json:"model"`
	Serial      string        `json:"serial"`
	Filesystem  string        `json:"fstype"`
	Mountpoints []*string     `json:"mountpoints"`
	ReadOnly    bool          `json:"ro"`
	Children    []blockDevice `json:"children"`
}

func diskUnavailable(d blockDevice) string {
	if d.ReadOnly {
		return "只读磁盘"
	}
	if slices.ContainsFunc(d.Mountpoints, func(p *string) bool { return p != nil && *p != "" }) {
		return "已挂载"
	}
	if d.Filesystem != "" {
		return "已有文件系统"
	}
	if len(d.Children) > 0 {
		return "已有分区或逻辑卷"
	}
	return ""
}

func (e *Engine) storageDevicePath() string {
	return filepath.Join(e.cfg.DataDir, "storage-device.json")
}

func (e *Engine) storageSelection() (string, error) {
	raw, err := os.ReadFile(e.storageDevicePath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var input api.ConfigureNodeStorage
	err = json.Unmarshal(raw, &input)
	return input.Device, err
}

func (e *Engine) initializeStorageSelection() error {
	// Import the installed startup setting once; subsequent changes use the persisted selection.
	if e.cfg.StorageDevice == "" {
		return nil
	}
	if _, err := os.Stat(e.storageDevicePath()); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return e.saveStorageSelection(api.ConfigureNodeStorage{Device: e.cfg.StorageDevice})
}

func (e *Engine) StorageDevices(ctx context.Context) (api.NodeStorageDevices, error) {
	selected, err := e.storageSelection()
	if err != nil {
		return api.NodeStorageDevices{}, err
	}
	out, err := commandOutput(ctx, "lsblk", "-J", "-b", "-p", "-o", "NAME,TYPE,SIZE,MODEL,SERIAL,FSTYPE,MOUNTPOINTS,RO")
	if err != nil {
		return api.NodeStorageDevices{}, err
	}
	var devices struct {
		Devices []blockDevice `json:"blockdevices"`
	}
	if err = json.Unmarshal(out, &devices); err != nil {
		return api.NodeStorageDevices{}, err
	}
	aliases, err := filepath.Glob("/dev/disk/by-id/*")
	if err != nil {
		return api.NodeStorageDevices{}, err
	}
	stable := map[string]string{}
	for _, alias := range aliases {
		if strings.Contains(filepath.Base(alias), "-part") {
			continue
		}
		target, resolveErr := filepath.EvalSymlinks(alias)
		if resolveErr == nil && stable[target] == "" {
			stable[target] = alias
		}
	}
	result := api.NodeStorageDevices{Selected: selected, Devices: []api.StorageDevice{}}
	for _, disk := range devices.Devices {
		if disk.Type != "disk" {
			continue
		}
		path := disk.Name
		if stable[path] != "" {
			path = stable[path]
		}
		reason := diskUnavailable(disk)
		if reason == "" {
			out, err = commandOutput(ctx, "wipefs", "-n", "-J", disk.Name)
			if err != nil {
				return result, err
			}
			var signatures struct {
				Signatures []json.RawMessage `json:"signatures"`
			}
			if err = json.Unmarshal(out, &signatures); err != nil {
				return result, err
			}
			if len(signatures.Signatures) > 0 {
				reason = "已有数据签名"
			}
		}
		result.Devices = append(result.Devices, api.StorageDevice{Path: path, Model: strings.TrimSpace(disk.Model), Serial: strings.TrimSpace(disk.Serial), SizeBytes: disk.Size, Available: reason == "", Reason: reason})
	}
	slices.SortFunc(result.Devices, func(a, b api.StorageDevice) int {
		if a.SizeBytes > b.SizeBytes {
			return -1
		}
		if a.SizeBytes < b.SizeBytes {
			return 1
		}
		return strings.Compare(a.Path, b.Path)
	})
	return result, nil
}

func (e *Engine) ConfigureStorageDevice(ctx context.Context, input api.ConfigureNodeStorage) (api.NodeInfo, error) {
	unlock := e.lock("storage-device")
	defer unlock()
	current, err := e.storageSelection()
	if err != nil {
		return api.NodeInfo{}, err
	}
	if current == input.Device {
		return e.Info()
	}
	if current != "" {
		out, err := commandOutput(ctx, "cephadm", "ls")
		if err != nil {
			return api.NodeInfo{}, err
		}
		var daemons []json.RawMessage
		if err = json.Unmarshal(out, &daemons); err != nil {
			return api.NodeInfo{}, err
		}
		if len(daemons) > 0 {
			return api.NodeInfo{}, errors.New("此节点的 Ceph 服务仍在使用专用盘")
		}
	}
	clusters, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "ceph"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return api.NodeInfo{}, err
	}
	if len(clusters) > 0 {
		return api.NodeInfo{}, errors.New("专用盘已参与 Ceph 配置；先移除共享池再调整选盘")
	}
	if input.Device != "" {
		inventory, err := e.StorageDevices(ctx)
		if err != nil {
			return api.NodeInfo{}, err
		}
		if !slices.ContainsFunc(inventory.Devices, func(d api.StorageDevice) bool { return d.Path == input.Device && d.Available }) {
			return api.NodeInfo{}, fmt.Errorf("%s 不是可用空盘", input.Device)
		}
		if err = command(ctx, "apt-get", "update"); err != nil {
			return api.NodeInfo{}, err
		}
		if err = command(ctx, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "cephadm", "podman", "openssh-server", "lvm2", "chrony"); err != nil {
			return api.NodeInfo{}, err
		}
		if err = command(ctx, "systemctl", "enable", "--now", "ssh", "chrony"); err != nil {
			return api.NodeInfo{}, err
		}
	}
	if err = e.saveStorageSelection(input); err != nil {
		return api.NodeInfo{}, err
	}
	return e.Info()
}

func (e *Engine) saveStorageSelection(input api.ConfigureNodeStorage) error {
	path := e.storageDevicePath()
	if err := writeRecoveryJSON(path+".next", input); err != nil {
		return err
	}
	return os.Rename(path+".next", path)
}
