//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

type storageFootprint struct {
	paths          map[string]bool
	disks          map[string]vmDisk
	parts          map[string]api.StorageUsagePart
	bases          map[string]bool
	baseDisks      map[string]bool
	measuredImages map[string]bool
	files          map[string][]allocatedFile
	pools          map[string]api.StorageInfo
}

func (e *Engine) StorageUsage(ctx context.Context, input api.NodeStorageUsage) ([]api.StorageUsagePart, error) {
	usage := storageFootprint{
		paths: map[string]bool{}, disks: map[string]vmDisk{}, parts: map[string]api.StorageUsagePart{},
		bases: map[string]bool{}, baseDisks: map[string]bool{}, measuredImages: map[string]bool{},
		files: map[string][]allocatedFile{}, pools: map[string]api.StorageInfo{},
	}
	ids := []string{}
	if input.Environments != nil {
		for id := range *input.Environments {
			if id != input.EnvironmentId {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	ids = append([]string{input.EnvironmentId}, ids...)
	// Count shared blocks in the current environment, then attribute each additional physical block once.
	previous := map[string]api.StorageUsagePart{}
	result := []api.StorageUsagePart{}
	for _, id := range ids {
		sources := input.Sources
		if id != input.EnvironmentId {
			sources = (*input.Environments)[id]
		}
		parts, err := e.storageFootprint(ctx, api.NodeStorageUsage{EnvironmentId: id, Sources: sources}, &usage)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			before := previous[part.Key]
			previous[part.Key] = part
			part.Bytes = max(int64(0), part.Bytes-before.Bytes)
			part.BaseBytes = min(part.Bytes, max(int64(0), part.BaseBytes-before.BaseBytes))
			if part.Bytes == 0 {
				continue
			}
			part.EnvironmentId = id
			if strings.HasPrefix(part.Key, e.cfg.ID+"/filesystem/") {
				part.Key += "/" + id
			}
			result = append(result, part)
		}
	}
	return result, nil
}

func (e *Engine) storageFootprint(ctx context.Context, input api.NodeStorageUsage, usage *storageFootprint) ([]api.StorageUsagePart, error) {
	paths, disks, parts := usage.paths, usage.disks, usage.parts
	bases, baseDisks, measuredImages := usage.bases, usage.baseDisks, usage.measuredImages
	paths[filepath.Join(e.cfg.DataDir, "packets", input.EnvironmentId)] = true
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
	baseUsage, err := allocatedFiles(ctx, bases, usage.files)
	if err != nil {
		return nil, err
	}
	for path := range bases {
		paths[path] = true
	}
	local, err := allocatedFiles(ctx, paths, usage.files)
	if err != nil {
		return nil, err
	}
	for fs, part := range local {
		part.Key = e.cfg.ID + "/filesystem/" + fs
		part.Filesystem = e.cfg.ID + "/" + fs
		part.BaseBytes = min(part.Bytes, baseUsage[fs].Bytes)
		parts[part.Key] = part
	}
	shared := usage.pools
	for key, disk := range disks {
		if _, measured := parts[key]; measured {
			continue
		}
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

type allocatedFile struct {
	path string
	stat unix.Stat_t
}

// Count file blocks once per inode; Btrfs measures the extent union, including compression and reflinks.
func allocatedFiles(ctx context.Context, paths map[string]bool, cache map[string][]allocatedFile) (map[string]api.StorageUsagePart, error) {
	seen := map[[2]uint64]bool{}
	sizes := map[string]api.StorageUsagePart{}
	native := map[string]map[string]bool{}
	filesystems := map[uint64]bool{}
	for root := range paths {
		files, known := cache[root]
		if !known {
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
				if err = unix.Stat(path, &stat); errors.Is(err, os.ErrNotExist) {
					return nil
				} else if err != nil {
					return err
				}
				files = append(files, allocatedFile{path, stat})
				return nil
			})
			if err != nil {
				return nil, err
			}
			cache[root] = files
		}
		var rootStat unix.Stat_t
		if len(files) > 0 {
			if err := unix.Stat(root, &rootStat); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		for _, file := range files {
			path, stat := file.path, file.stat
			identity := [2]uint64{stat.Dev, stat.Ino}
			if seen[identity] {
				continue
			}
			seen[identity] = true
			key := strconv.FormatUint(stat.Dev, 10)
			cow, known := filesystems[stat.Dev]
			if !known {
				storage, err := StorageInfo(path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return nil, err
				}
				cow = storage.NativeSnapshots
				filesystems[stat.Dev] = cow
				sizes[key] = api.StorageUsagePart{Storage: &storage}
			}
			if cow {
				if native[key] == nil {
					native[key] = map[string]bool{}
				}
				candidate := path
				if rootStat.Dev == stat.Dev {
					candidate = root
				}
				native[key][candidate] = true
			} else {
				part := sizes[key]
				part.Bytes += stat.Blocks * 512
				sizes[key] = part
			}
		}
	}
	for key, roots := range native {
		files := make([]string, 0, len(roots))
		for root := range roots {
			files = append(files, root)
		}
		slices.SortFunc(files, func(a, b string) int { return strings.Compare(a+string(os.PathSeparator), b+string(os.PathSeparator)) })
		minimal := files[:0]
		for _, path := range files {
			if len(minimal) > 0 && strings.HasPrefix(path, minimal[len(minimal)-1]+string(os.PathSeparator)) {
				continue
			}
			minimal = append(minimal, path)
		}
		// Directory roots keep packet segments out of the process argument list.
		out, err := commandOutput(ctx, "compsize", append([]string{"-b", "-x", "--"}, minimal...)...)
		if err != nil {
			// compsize reports zero allocated extents with exit status 1.
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(string(exit.Stderr)) == "All empty or still-delalloced files." {
				continue
			}
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
