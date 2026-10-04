//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

func StorageInfo(path string) (api.StorageInfo, error) {
	var filesystem unix.Statfs_t
	if err := unix.Statfs(path, &filesystem); err != nil {
		return api.StorageInfo{}, err
	}
	var file unix.Stat_t
	if err := unix.Stat(path, &file); err != nil {
		return api.StorageInfo{}, err
	}
	return api.StorageInfo{Path: path, Filesystem: strconv.FormatUint(file.Dev, 10), CapacityBytes: int64(filesystem.Blocks) * int64(filesystem.Bsize), AvailableBytes: int64(filesystem.Bavail) * int64(filesystem.Bsize), NativeSnapshots: filesystem.Type == unix.BTRFS_SUPER_MAGIC}, nil
}

func cloneFile(source, target string) (err error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(output.Fd()), int(input.Fd()))
	return errors.Join(err, output.Close())
}

func (e *Engine) RegisterStorage(ctx context.Context, id string, input api.CreateStoragePool) (api.StorageInfo, error) {
	switch input.Driver {
	case api.StorageDriverDirectory:
		if input.Directory == nil || input.Ceph != nil || len(input.NodeIds) != 1 {
			return api.StorageInfo{}, errors.New("目录存储需要一个节点和目录")
		}
		return e.registerDirectory(id, *input.Directory)
	case api.StorageDriverRBD:
		if e.vm == nil || input.Ceph == nil || input.Directory != nil {
			return api.StorageInfo{}, errors.New("RBD 存储需要虚拟机执行器和 Ceph 连接")
		}
		root := filepath.Join(e.cfg.DataDir, "storage-pools", id)
		if err := os.MkdirAll(root, 0711); err != nil {
			return api.StorageInfo{}, err
		}
		return e.registerRBD(ctx, id, root, *input.Ceph)
	default:
		return api.StorageInfo{}, errors.New("未知存储类型")
	}
}

func (e *Engine) Storage(ctx context.Context, id, directory string) (api.StorageInfo, error) {
	if directory != "" {
		return StorageInfo(e.storagePath(id, directory))
	}
	var info api.StorageInfo
	raw, err := os.ReadFile(filepath.Join(e.cfg.DataDir, "storage-pools", id, "storage.json"))
	if err != nil {
		return info, err
	}
	if err = json.Unmarshal(raw, &info); err != nil {
		return info, err
	}
	return rbdStorageInfo(ctx, info.Path, *info.Rbd)
}

func (e *Engine) registerDirectory(id, directory string) (api.StorageInfo, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return api.StorageInfo{}, errors.New("存储目录应为绝对路径")
	}
	info, err := os.Stat(directory)
	if err != nil {
		return api.StorageInfo{}, err
	}
	if !info.IsDir() {
		return api.StorageInfo{}, errors.New("存储位置不是目录")
	}
	path := e.storagePath(id, directory)
	if err = os.MkdirAll(path, 0711); err != nil {
		return api.StorageInfo{}, err
	}
	return StorageInfo(path)
}

func (e *Engine) storagePath(id, directory string) string {
	return filepath.Join(directory, "netlab-"+e.cfg.ID, id)
}

func (e *Engine) RemoveStorage(ctx context.Context, id, directory string) error {
	root := filepath.Join(e.cfg.DataDir, "storage-pools", id)
	if directory != "" {
		return removeEmptyDirectories(e.storagePath(id, directory))
	}
	info, err := e.Storage(ctx, id, "")
	if errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(root)
	}
	if err != nil {
		return err
	}
	if err = e.removeRBD(ctx, info); err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func removeEmptyDirectories(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("存储池仍包含数据：%s", entry.Name())
		}
		if err = removeEmptyDirectories(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func assetDirectory(data, env string, a api.AssetExecution) string {
	id := a.InstanceId
	if a.DataSetId != "" {
		id += "." + a.DataSetId
	}
	return instanceDir(storageRoot(data, a), env, id)
}
