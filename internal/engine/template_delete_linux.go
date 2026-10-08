//go:build linux

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
)

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
