//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"netlab.local/core/api"
)

func (e *Engine) TemplateCache(ctx context.Context, id string, version int) (api.TemplateCache, error) {
	result := api.TemplateCache{NodeId: e.cfg.ID, NodeName: e.cfg.Name}
	directory := templateDirectory(e.cfg.DataDir, id, version)
	raw, err := os.ReadFile(filepath.Join(directory, "template.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var template api.Template
	if err = json.Unmarshal(raw, &template); err != nil {
		return result, err
	}
	if template.Kind != api.Vm || template.Disks == nil {
		return result, nil
	}
	for i := range *template.Disks {
		info, err := os.Stat(systemDiskPath(directory, i))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, err
		}
		result.Bytes += info.Sys().(*syscall.Stat_t).Blocks * 512
	}
	result.Reclaimable = result.Bytes > 0
	if e.vm != nil {
		inventory, err := e.vm.Inventory(ctx, "")
		if err != nil {
			return result, err
		}
		for _, asset := range inventory {
			if asset.Execution != nil && asset.Execution.Template.Id == id && asset.Execution.Rbd == nil {
				result.Reclaimable = false
				result.Reason = "本地虚拟机正在引用"
				break
			}
		}
	}
	return result, nil
}

func (e *Engine) TrimTemplateCache(ctx context.Context, input api.TemplateCacheRequest) (api.TemplateCache, error) {
	t := input.Template
	unlock := e.lock("template:" + t.Id)
	defer unlock()
	result, err := e.TemplateCache(ctx, t.Id, t.Version)
	if err != nil || result.Bytes == 0 {
		return result, err
	}
	if !result.Reclaimable {
		return result, errors.New(result.Reason)
	}
	for index := range *t.Disks {
		shared := false
		for _, id := range input.PoolIds {
			storage, err := e.Storage(ctx, id, "")
			if err != nil {
				return result, err
			}
			if storage.Rbd == nil {
				continue
			}
			exists, err := sharedTemplateDisk(storage.Path, *storage.Rbd, t, index).exists(ctx)
			if err != nil {
				return result, err
			}
			shared = shared || exists
		}
		if !shared {
			return result, fmt.Errorf("磁盘 %d 尚未存入共享池", index+1)
		}
	}
	unlockArtifact := e.lock(fmt.Sprintf("artifact:%s:%d", t.Id, t.Version))
	defer unlockArtifact()
	unlockReader := e.lock(fmt.Sprintf("template-reader:%s:%d", t.Id, t.Version))
	defer unlockReader()
	for i := range *t.Disks {
		if err = os.Remove(systemDiskPath(templateDirectory(e.cfg.DataDir, t.Id, t.Version), i)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	return e.TemplateCache(ctx, t.Id, t.Version)
}

func (e *Engine) hydrateTemplate(ctx context.Context, t api.Template) error {
	if t.Kind != api.Vm || t.Disks == nil {
		return nil
	}
	directory := templateDirectory(e.cfg.DataDir, t.Id, t.Version)
	for i := range *t.Disks {
		path := systemDiskPath(directory, i)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		pools, err := os.ReadDir(filepath.Join(e.cfg.DataDir, "storage-pools"))
		if err != nil {
			return err
		}
		restored := false
		for _, pool := range pools {
			storage, err := e.Storage(ctx, pool.Name(), "")
			if err != nil {
				return err
			}
			if storage.Rbd == nil {
				continue
			}
			base := sharedTemplateDisk(storage.Path, *storage.Rbd, t, i)
			exists, err := base.exists(ctx)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			if err = command(ctx, "qemu-img", "convert", "-f", "raw", "-O", "qcow2", base.address(), path+".pending"); err != nil {
				return err
			}
			if err = os.Rename(path+".pending", path); err != nil {
				return err
			}
			restored = true
			break
		}
		if !restored {
			return fmt.Errorf("模板磁盘 %d 在本地与共享池均不存在", i+1)
		}
	}
	return nil
}
