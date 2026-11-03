//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"netlab.local/core/api"
)

// QEMU determines sparse metadata costs; existing disks are counted by filesystem free space.
func (e *Engine) EstimateStorage(ctx context.Context, input api.NodeStorageEstimate) (map[string][]api.StorageUsagePart, error) {
	result := map[string][]api.StorageUsagePart{}
	measured := map[int64]int64{}
	for key, a := range input.Assets {
		if a.Template.Kind != api.Vm {
			return nil, errors.New("sparse disk estimation requires a VM")
		}
		filesystem := e.cfg.ID + "/" + *a.StorageFilesystem
		if a.Rbd != nil {
			filesystem = *a.StorageFilesystem
		}
		var required int64
		sizes := []int64{}
		if a.Template.Disks != nil {
			for i, disk := range *a.Template.Disks {
				size := disk.SizeGiB
				if i == 0 {
					size += max(int64(0), a.Asset.Resources.DiskGiB-a.Template.Resources.DiskGiB)
				}
				sizes = append(sizes, size)
			}
		} else {
			sizes = append(sizes, a.Asset.Resources.DiskGiB)
		}
		if a.Asset.Volumes != nil {
			for _, volume := range *a.Asset.Volumes {
				if volume.PersistentVolumeId == nil {
					sizes = append(sizes, volume.SizeGiB)
				}
			}
		}
		for _, size := range sizes {
			bytes, ok := measured[size]
			if !ok {
				out, err := commandOutput(ctx, "qemu-img", "measure", "--output=json", "-O", "qcow2", "--size", strconv.FormatInt(size<<30, 10))
				if err != nil {
					return nil, err
				}
				var measure struct {
					Required int64 `json:"required"`
				}
				if err = json.Unmarshal(out, &measure); err != nil {
					return nil, err
				}
				bytes = measure.Required
				measured[size] = bytes
			}
			required += bytes
		}
		parts := []api.StorageUsagePart{{Key: filesystem, Bytes: required}}
		if a.Template.Id != "" {
			cached := filepath.Join(templateDirectory(e.cfg.DataDir, a.Template.Id, a.Template.Version), "template.json")
			_, err := os.Stat(cached)
			if errors.Is(err, os.ErrNotExist) {
				local, err := StorageInfo(e.cfg.DataDir)
				if err != nil {
					return nil, err
				}
				// A cold node retains source and conversion space until import completes.
				parts = append(parts, api.StorageUsagePart{Key: e.cfg.ID + "/" + local.Filesystem, Bytes: 2 * a.Template.Resources.DiskGiB << 30})
			} else if err != nil {
				return nil, err
			}
			if a.Rbd != nil && a.Template.Disks != nil {
				for i, disk := range *a.Template.Disks {
					exists, err := sharedTemplateDisk(*a.StoragePath, *a.Rbd, a.Template, i).exists(ctx)
					if err != nil {
						return nil, fmt.Errorf("estimate shared template %s: %w", a.Template.Id, err)
					}
					if !exists {
						parts[0].Bytes += disk.SizeGiB << 30
					}
				}
			}
		}
		result[key] = parts
	}
	return result, nil
}
