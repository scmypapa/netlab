//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/mount"
	"golang.org/x/sys/unix"
)

// A bounded native filesystem makes the volume's declared capacity an actual write limit.
func prepareDirectoryVolume(ctx context.Context, path string, sizeGiB int64) error {
	image := path + ".ext4"
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	info, err := os.Stat(image)
	if errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("数据卷 %s 已包含未登记的文件", filepath.Base(path))
		}
		staging := image + ".pending"
		file, err := os.OpenFile(staging, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer os.Remove(staging)
		if err = errors.Join(file.Truncate(sizeGiB<<30), file.Close()); err != nil {
			return err
		}
		if err = command(ctx, "mkfs.ext4", "-q", "-m", "0", staging); err != nil {
			return err
		}
		if err = os.Rename(staging, image); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if info.Size() > sizeGiB<<30 {
		return errors.New("数据卷不能缩小")
	} else if info.Size() < sizeGiB<<30 {
		if err = os.Truncate(image, sizeGiB<<30); err != nil {
			return err
		}
	}
	current, err := mount.Lookup(path)
	if err != nil {
		return err
	}
	if current.Mountpoint != path {
		if err = command(ctx, "resize2fs", image); err != nil {
			return err
		}
		return command(ctx, "mount", "-o", "loop,nodev,nosuid,noatime", image, path)
	}
	if err = command(ctx, "losetup", "--set-capacity", current.Source); err != nil {
		return err
	}
	return command(ctx, "resize2fs", current.Source)
}

func removeDirectoryVolume(path string) error {
	if _, err := os.Stat(path); err == nil {
		current, err := mount.Lookup(path)
		if err != nil {
			return err
		}
		if current.Mountpoint == path {
			if err = unix.Unmount(path, 0); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	for _, file := range []string{path + ".ext4", path + ".ext4.pending"} {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func removeDirectoryVolumes(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err = removeDirectoryVolume(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(root)
}
