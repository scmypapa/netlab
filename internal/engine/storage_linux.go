//go:build linux

package engine

import (
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
	return api.StorageInfo{Path: path, Filesystem: strconv.FormatUint(file.Dev, 10), CapacityBytes: int64(filesystem.Blocks) * int64(filesystem.Bsize), AvailableBytes: int64(filesystem.Bavail) * int64(filesystem.Bsize)}, nil
}

func (e *Engine) RegisterStorage(id, directory string) (api.StorageInfo, error) {
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

func (e *Engine) RemoveStorage(id, directory string) error {
	path := e.storagePath(id, directory)
	// Removing empty directories preserves retained volumes and unexpected files.
	return removeEmptyDirectories(path)
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

func storageRoot(data string, a api.AssetExecution) string {
	if a.StoragePath != nil {
		return *a.StoragePath
	}
	return data
}

func assetDirectory(data, env string, a api.AssetExecution) string {
	return instanceDir(storageRoot(data, a), env, a.InstanceId)
}
