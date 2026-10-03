//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/continuity/fs"
	"netlab.local/core/internal/files"
)

func (e *Engine) ContainerFiles(w http.ResponseWriter, r *http.Request) {
	env, asset, instance := r.PathValue("environmentId"), r.PathValue("assetId"), r.PathValue("instanceId")
	unlock := e.lock(env + "/" + asset)
	defer unlock()
	if e.container == nil {
		files.Failure(w, errors.New("节点未启用容器运行时"), http.StatusConflict)
		return
	}
	err := e.container.withFiles(r.Context(), env, asset, instance, func(store files.Store) { files.Serve(w, r, store) })
	if err != nil {
		files.Failure(w, err, http.StatusUnprocessableEntity)
	}
}

func (c *Containers) withFiles(ctx context.Context, env, asset, instance string, use func(files.Store)) error {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	container, err := c.client.LoadContainer(ctx, instance)
	if err != nil {
		return err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return err
	}
	if labels[environmentLabel] != env || labels[assetLabel] != asset {
		return errors.New("容器归属不匹配")
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return err
	}
	withRoot := func(root string) error {
		store, err := files.OpenRoot(root, int(spec.Process.User.UID), int(spec.Process.User.GID))
		if err != nil {
			return err
		}
		defer store.Close()
		use(store)
		return nil
	}
	task, err := container.Task(ctx, nil)
	if err == nil {
		status, statusErr := task.Status(ctx)
		if statusErr != nil {
			return statusErr
		}
		if status.Status == containerd.Running || status.Status == containerd.Paused {
			return withRoot(fmt.Sprintf("/proc/%d/root", task.Pid()))
		}
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return err
	}
	mounts, err := c.client.SnapshotService(info.Snapshotter).Mounts(ctx, info.SnapshotKey)
	if err != nil {
		return err
	}
	return mount.WithTempMount(ctx, mounts, func(root string) error {
		var mounted []string
		defer func() {
			for index := len(mounted) - 1; index >= 0; index-- {
				mount.Unmount(mounted[index], 0)
			}
		}()
		for _, item := range spec.Mounts {
			if item.Type != "bind" {
				continue
			}
			target, err := fs.RootPath(root, filepath.Clean(item.Destination))
			if err != nil {
				return err
			}
			if err = mount.All([]mount.Mount{{Type: "bind", Source: item.Source, Options: item.Options}}, target); err != nil {
				return err
			}
			mounted = append(mounted, target)
		}
		return withRoot(root)
	})
}
