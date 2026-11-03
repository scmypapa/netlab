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
	"strings"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

func (e *Engine) StorageUsage(ctx context.Context, input api.NodeStorageUsage) ([]api.StorageUsagePart, error) {
	paths, disks := map[string]bool{}, map[string]vmDisk{}
	parts := map[string]int64{}
	for _, source := range input.Sources {
		a := source.Execution
		if source.RecoveryId != "" {
			directory := recoveryDirectory(e.cfg.DataDir, source.RecoveryId, a)
			paths[directory] = true
			paths[directory+".pending"] = true
			if a.Rbd != nil {
				raw, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return nil, err
				}
				var manifest recoveryManifest
				if err = json.Unmarshal(raw, &manifest); err != nil {
					return nil, err
				}
				for _, captured := range manifest.Disks {
					if captured.RBDImage != "" {
						d := vmDisk{image: captured.RBDImage, rbd: a.Rbd, root: *a.StoragePath}
						disks[d.key()] = d
					}
				}
			}
			continue
		}
		paths[assetDirectory(e.cfg.DataDir, input.EnvironmentId, a)] = true
		if a.Template.Kind == api.Vm {
			for _, d := range executionDisks(e.cfg.DataDir, input.EnvironmentId, a) {
				if d.rbd != nil {
					disks[d.key()] = d
				} else {
					paths[d.file] = true
				}
			}
			continue
		}
		if e.container == nil {
			return nil, errors.New("container runtime not configured")
		}
		containerCtx := namespaces.WithNamespace(ctx, "netlab")
		container, err := e.container.client.LoadContainer(containerCtx, a.InstanceId)
		if err != nil && !errdefs.IsNotFound(err) {
			return nil, err
		}
		if err == nil {
			info, err := container.Info(containerCtx)
			if err != nil {
				return nil, err
			}
			usage, err := e.container.client.SnapshotService(info.Snapshotter).Usage(containerCtx, info.SnapshotKey)
			if err != nil {
				return nil, err
			}
			parts[e.cfg.ID+"/container/"+a.InstanceId] = usage.Size
		}
		if a.Asset.Volumes != nil {
			for _, v := range *a.Asset.Volumes {
				paths[e.container.volumeDir(input.EnvironmentId, a, v.Id)+".ext4"] = true
			}
		}
	}
	local, err := allocatedFiles(ctx, paths)
	if err != nil {
		return nil, err
	}
	for key, size := range local {
		parts[e.cfg.ID+"/filesystem/"+key] = size
	}
	for key, disk := range disks {
		size, err := disk.usedBytes(ctx)
		if err != nil {
			return nil, err
		}
		parts[key] = size
	}
	result := make([]api.StorageUsagePart, 0, len(parts))
	for key, size := range parts {
		result = append(result, api.StorageUsagePart{Key: key, Bytes: size})
	}
	return result, nil
}

// Count file blocks once per inode; Btrfs measures the extent union, including compression and reflinks.
func allocatedFiles(ctx context.Context, paths map[string]bool) (map[string]int64, error) {
	seen := map[[2]uint64]bool{}
	sizes := map[string]int64{}
	native := map[string][]string{}
	filesystems := map[uint64]bool{}
	for root := range paths {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			var stat unix.Stat_t
			if err = unix.Stat(path, &stat); err != nil {
				return err
			}
			identity := [2]uint64{stat.Dev, stat.Ino}
			if seen[identity] {
				return nil
			}
			seen[identity] = true
			cow, known := filesystems[stat.Dev]
			if !known {
				var fs unix.Statfs_t
				if err = unix.Statfs(path, &fs); err != nil {
					return err
				}
				cow = fs.Type == unix.BTRFS_SUPER_MAGIC
				filesystems[stat.Dev] = cow
			}
			key := strconv.FormatUint(stat.Dev, 10)
			if cow {
				native[key] = append(native[key], path)
			} else {
				sizes[key] += stat.Blocks * 512
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for key, files := range native {
		out, err := commandOutput(ctx, "compsize", append([]string{"-b", "--"}, files...)...)
		if err != nil {
			return nil, fmt.Errorf("measure Btrfs storage: %w", err)
		}
		found := false
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == "TOTAL" {
				sizes[key], err = strconv.ParseInt(fields[2], 10, 64)
				if err != nil {
					return nil, err
				}
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("compsize did not return allocated bytes")
		}
	}
	return sizes, nil
}
