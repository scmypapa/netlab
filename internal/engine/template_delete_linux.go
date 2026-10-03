//go:build linux

package engine

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
)

func (e *Engine) RemoveTemplate(ctx context.Context, id string, version int) error {
	unlock := e.lock("template:" + id)
	defer unlock()
	if e.container != nil {
		ctx = namespaces.WithNamespace(ctx, "netlab")
		ref := fmt.Sprintf("netlab/template/%s:%d", id, version)
		if err := e.container.client.ImageService().Delete(ctx, ref); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if err := os.RemoveAll(templateDirectory(e.cfg.DataDir, id, version)); err != nil {
		return err
	}
	return os.RemoveAll(importDirectory(e.cfg.DataDir, id, version))
}
