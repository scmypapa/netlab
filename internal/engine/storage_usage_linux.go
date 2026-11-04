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
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/opencontainers/image-spec/identity"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

func (e *Engine) StorageUsage(ctx context.Context, input api.NodeStorageUsage) ([]api.StorageUsagePart, error) {
	paths, disks := map[string]bool{}, map[string]vmDisk{}
	paths[filepath.Join(e.cfg.DataDir, "packets", input.EnvironmentId)] = true
	parts := map[string]api.StorageUsagePart{}
	bases := map[string]bool{}
	baseDisks := map[string]bool{}
	measuredImages := map[string]bool{}
	for _, source := range input.Sources {
		a := source.Execution
		directory := templateDirectory(e.cfg.DataDir, a.Template.Id, a.Template.Version)
		for _, file := range templateArtifactFiles(a.Template, a.Rbd != nil) {
			bases[filepath.Join(directory, file)] = true
		}
		if a.Template.Kind == api.Vm {
			if a.Rbd != nil && a.Template.Disks != nil {
				for i := range *a.Template.Disks {
					d := sharedTemplateDisk(*a.StoragePath, *a.Rbd, a.Template, i)
					disks[d.key()] = d
					baseDisks[d.key()] = true
				}
			}
		} else if !measuredImages[fmt.Sprintf("%s/%d", a.Template.Id, a.Template.Version)] {
			if e.container == nil {
				return nil, errors.New("container runtime not configured")
			}
			if err := e.container.imageStorageUsage(ctx, a.Template, bases, parts); err != nil {
				return nil, err
			}
			measuredImages[fmt.Sprintf("%s/%d", a.Template.Id, a.Template.Version)] = true
		}
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
			storage, err := e.container.Storage(ctx)
			if err != nil {
				return nil, err
			}
			key := e.cfg.ID + "/snapshot/" + info.Snapshotter + "/" + info.SnapshotKey
			parts[key] = api.StorageUsagePart{Key: key, Bytes: usage.Size, Filesystem: e.cfg.ID + "/" + storage.Filesystem, Storage: &storage}
		}
		if a.Asset.Volumes != nil {
			for _, v := range *a.Asset.Volumes {
				paths[e.container.volumeDir(input.EnvironmentId, a, v.Id)+".ext4"] = true
			}
		}
	}
	baseUsage, err := allocatedFiles(ctx, bases)
	if err != nil {
		return nil, err
	}
	for path := range bases {
		paths[path] = true
	}
	local, err := allocatedFiles(ctx, paths)
	if err != nil {
		return nil, err
	}
	for fs, part := range local {
		part.Key = e.cfg.ID + "/filesystem/" + fs
		part.Filesystem = e.cfg.ID + "/" + fs
		part.BaseBytes = min(part.Bytes, baseUsage[fs].Bytes)
		parts[part.Key] = part
	}
	shared := map[string]api.StorageInfo{}
	for key, disk := range disks {
		size, err := disk.usedBytes(ctx)
		if err != nil {
			return nil, err
		}
		fs := disk.rbd.Fsid + "/" + disk.rbd.Pool
		storage, known := shared[fs]
		if !known {
			storage, err = rbdStorageInfo(ctx, disk.root, *disk.rbd)
			if err != nil {
				return nil, err
			}
			shared[fs] = storage
		}
		part := api.StorageUsagePart{Key: key, Bytes: size, Filesystem: fs, Storage: &storage}
		if baseDisks[key] {
			part.BaseBytes = size
		}
		parts[key] = part
	}
	result := make([]api.StorageUsagePart, 0, len(parts))
	for _, part := range parts {
		result = append(result, part)
	}
	return result, nil
}

// Count file blocks once per inode; Btrfs measures the extent union, including compression and reflinks.
func allocatedFiles(ctx context.Context, paths map[string]bool) (map[string]api.StorageUsagePart, error) {
	seen := map[[2]uint64]bool{}
	sizes := map[string]api.StorageUsagePart{}
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
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			identity := [2]uint64{stat.Dev, stat.Ino}
			if seen[identity] {
				return nil
			}
			seen[identity] = true
			key := strconv.FormatUint(stat.Dev, 10)
			cow, known := filesystems[stat.Dev]
			if !known {
				storage, err := StorageInfo(path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return nil
					}
					return err
				}
				cow = storage.NativeSnapshots
				filesystems[stat.Dev] = cow
				sizes[key] = api.StorageUsagePart{Storage: &storage}
			}
			if cow {
				native[key] = append(native[key], path)
			} else {
				part := sizes[key]
				part.Bytes += stat.Blocks * 512
				sizes[key] = part
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
				part := sizes[key]
				part.Bytes, err = strconv.ParseInt(fields[2], 10, 64)
				sizes[key] = part
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

// Containerd retains both immutable snapshots and compressed content; shared layers are counted by their native identity.
func (c *Containers) imageStorageUsage(ctx context.Context, template api.Template, files map[string]bool, parts map[string]api.StorageUsagePart) error {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	image, err := c.client.GetImage(ctx, fmt.Sprintf("netlab/template/%s:%d", template.Id, template.Version))
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	storage, err := c.Storage(ctx)
	if err != nil {
		return err
	}
	roots, err := image.RootFS(ctx)
	if err != nil {
		return err
	}
	snapshots := c.client.SnapshotService("overlayfs")
	for _, chain := range identity.ChainIDs(roots) {
		key := chain.String()
		partKey := c.node + "/snapshot/overlayfs/" + key
		if _, counted := parts[partKey]; counted {
			continue
		}
		usage, err := snapshots.Usage(ctx, key)
		// Imported layers occupy content storage before they are unpacked.
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		parts[partKey] = api.StorageUsagePart{Key: partKey, Bytes: usage.Size, BaseBytes: usage.Size, Filesystem: c.node + "/" + storage.Filesystem, Storage: &storage}
	}
	plugins, err := c.client.IntrospectionService().Plugins(ctx, []string{"type==io.containerd.content.v1,id==content"})
	if err != nil {
		return err
	}
	if len(plugins.Plugins) == 0 || plugins.Plugins[0].Exports["root"] == "" {
		return errors.New("containerd content store did not publish its storage root")
	}
	root := plugins.Plugins[0].Exports["root"]
	handler := images.HandlerFunc(func(ctx context.Context, desc imagespec.Descriptor) ([]imagespec.Descriptor, error) {
		path := filepath.Join(root, "blobs", desc.Digest.Algorithm().String(), desc.Digest.Encoded())
		if files[path] {
			return nil, nil
		}
		files[path] = true
		return images.Children(ctx, c.client.ContentStore(), desc)
	})
	return images.Walk(ctx, images.FilterPlatforms(handler, image.Platform()), image.Target())
}
