//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"netlab.local/core/api"
)

func sharedTemplateDisk(root string, storage api.RbdStorage, t api.Template, index int) vmDisk {
	return vmDisk{root: root, rbd: &storage, image: fmt.Sprintf("%stemplate.%s.%d.disk-%d", storage.ImagePrefix, t.Id, t.Version, index), snapshot: "base"}
}

func (v *VirtualMachines) prepareSharedTemplate(ctx context.Context, t api.Template, storage api.StorageInfo) error {
	if storage.Rbd == nil {
		return errors.New("shared template preparation requires RBD storage")
	}
	for index, disk := range *t.Disks {
		base := sharedTemplateDisk(storage.Path, *storage.Rbd, t, index)
		exists, err := base.exists(ctx)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		staging := base
		staging.image += ".pending"
		staging.snapshot = ""
		if err = staging.removeTemplateBase(ctx); err != nil {
			return err
		}
		if err = staging.prepare(ctx, systemDiskPath(templateDirectory(v.data, t.Id, t.Version), index), disk.SizeGiB); err == nil {
			err = staging.capture(ctx, base.snapshot)
		}
		if err == nil {
			_, err = base.rbdCommand(ctx, "rename", staging.image, base.image)
		}
		if err != nil {
			return errors.Join(err, staging.removeTemplateBase(context.WithoutCancel(ctx)))
		}
	}
	return nil
}

func (d vmDisk) removeTemplateBase(ctx context.Context) error {
	exists, err := d.exists(ctx)
	if err != nil || !exists {
		return err
	}
	snapshots, err := d.snapshots(ctx)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		children, err := d.children(ctx, snapshot.Name)
		if err != nil {
			return err
		}
		if len(children) > 0 {
			return errors.New("基础镜像仍被实例引用")
		}
	}
	if _, err = d.rbdCommand(ctx, "snap", "purge", d.image); err != nil {
		return err
	}
	return d.remove(ctx)
}

func (e *Engine) removeSharedTemplate(ctx context.Context, id string, poolIDs []string) error {
	for _, poolID := range poolIDs {
		var storage api.StorageInfo
		raw, err := os.ReadFile(filepath.Join(e.cfg.DataDir, "storage-pools", poolID, "storage.json"))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &storage); err != nil {
			return err
		}
		disk := vmDisk{root: storage.Path, rbd: storage.Rbd}
		names, err := disk.rbdImages(ctx)
		if err != nil {
			return err
		}
		prefix := storage.Rbd.ImagePrefix + "template." + id + "."
		for _, name := range names {
			if strings.HasPrefix(name, prefix) {
				disk.image = name
				if err = disk.removeTemplateBase(ctx); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
