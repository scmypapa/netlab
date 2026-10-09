//go:build linux

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
)

func (e *Engine) CleanRetiredNode(ctx context.Context) error {
	actual, err := e.Inventory(ctx, "")
	if err != nil {
		return err
	}
	if len(actual.Results) != 0 {
		return errors.New("节点仍有运行实例，完成迁移后再退出")
	}
	ids := map[string]bool{}
	for _, name := range []string{"artifacts", "template-runtime", "imports"} {
		entries, err := os.ReadDir(filepath.Join(e.cfg.DataDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				ids[entry.Name()] = true
			}
		}
	}
	for id := range ids {
		if err = e.RemoveTemplate(ctx, id, nil); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) RemoveTemplate(ctx context.Context, id string, poolIDs []string) error {
	unlock := e.lock("template:" + id)
	defer unlock()
	if err := e.removeSharedTemplate(ctx, id, poolIDs); err != nil {
		return err
	}
	if e.container != nil {
		ctx = namespaces.WithNamespace(ctx, "netlab")
		images, err := e.container.client.ImageService().List(ctx)
		if err != nil {
			return err
		}
		for _, image := range images {
			if strings.HasPrefix(image.Name, "netlab/template/"+id+":") {
				if err = e.container.client.ImageService().Delete(ctx, image.Name); err != nil && !errdefs.IsNotFound(err) {
					return err
				}
			}
		}
	}
	for _, directory := range []string{"artifacts", "template-runtime", "imports"} {
		if err := os.RemoveAll(filepath.Join(e.cfg.DataDir, directory, id)); err != nil {
			return err
		}
	}
	return nil
}
