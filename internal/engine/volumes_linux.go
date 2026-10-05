//go:build linux

package engine

import (
	"context"
	"fmt"
	"path/filepath"

	"netlab.local/core/api"
)

func removeVolumeFiles(volumes *[]api.Volume, references map[string]bool, pathFor func(string) string, remove func(string) error, requireUnused bool) error {
	if volumes == nil {
		return nil
	}
	for _, volume := range *volumes {
		if volume.PersistentVolumeId != nil {
			continue
		}
		path := pathFor(volume.Id)
		if references[path] {
			if requireUnused {
				return fmt.Errorf("volume %s is still attached to an instance", volume.Id)
			}
			continue
		}
		if err := remove(volume.Id); err != nil {
			return err
		}
	}
	return nil
}

func persistentDirectory(volume api.NodeVolume) string {
	return filepath.Join(volume.Storage.Path, "volumes", volume.Id)
}

func (e *Engine) Volume(ctx context.Context, action string, volume api.NodeVolume) error {
	unlock := e.lock("volume:" + volume.Id)
	defer unlock()
	select {
	case e.ioSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.ioSlots }()
	if volume.Kind == api.Vm {
		disk := persistentDisk(volume)
		if action == "delete" {
			return disk.remove(ctx)
		}
		return disk.prepare(ctx, "", volume.SizeGiB)
	}
	if volume.Kind != api.Container || volume.Storage.Rbd != nil {
		return fmt.Errorf("目录数据卷需要目录存储")
	}
	path := persistentDirectory(volume)
	if action == "delete" {
		return removeDirectoryVolume(path)
	}
	return prepareDirectoryVolume(ctx, path, volume.SizeGiB)
}
