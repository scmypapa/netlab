//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/oci"
	"netlab.local/core/api"
)

const desiredLabel = "netlab.desired"
const restartErrorLabel = "netlab.restart-error"
const policiesLabel = "netlab.policies"

type containerWatch struct {
	mu      sync.Mutex
	pid     uint32
	started time.Time
	delay   time.Duration
}

func (c *Containers) watch(task containerd.Task) error {
	value, _ := c.watching.LoadOrStore(task.ID(), &containerWatch{})
	watch := value.(*containerWatch)
	watch.mu.Lock()
	defer watch.mu.Unlock()
	if watch.pid == task.Pid() {
		return nil
	}
	// Register Wait before Start, including for entrypoints that exit immediately.
	exited, err := task.Wait(c.ctx)
	if err != nil {
		return err
	}
	if time.Since(watch.started) >= 10*time.Second {
		watch.delay = 0
	}
	watch.delay = min(max(watch.delay*2, 100*time.Millisecond), 30*time.Second)
	watch.pid, watch.started = task.Pid(), time.Now()
	delay := watch.delay
	c.watches.Add(1)
	go func() {
		defer c.watches.Done()
		defer func() {
			watch.mu.Lock()
			defer watch.mu.Unlock()
			if watch.pid == task.Pid() {
				c.watching.CompareAndDelete(task.ID(), watch)
			}
		}()
		select {
		case <-c.ctx.Done():
			return
		case exit := <-exited:
			code, _, err := exit.Result()
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Error("container exit observation", "instance", task.ID(), "error", err)
				}
				return
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-c.ctx.Done():
				return
			case <-timer.C:
			}
			// The callback holds the same environment/asset locks as explicit operations.
			if err = c.restart(c.ctx, task.ID(), task.Pid(), code); err != nil {
				slog.Error("container restart", "instance", task.ID(), "error", err)
			}
		}
	}()
	return nil
}

func (e *Engine) superviseContainers() error {
	c := e.container
	c.restart = e.restartContainer
	items, err := c.client.Containers(c.ctx, "labels.\""+assetLabel+"\"")
	if err != nil {
		return err
	}
	for _, item := range items {
		labels, err := item.Labels(c.ctx)
		if err != nil {
			return err
		}
		spec, err := item.Spec(c.ctx)
		if err != nil {
			return err
		}
		if !managedContainer(spec, c.data, labels[environmentLabel], item.ID()) {
			continue
		}
		task, err := item.Task(c.ctx, nil)
		if errdefs.IsNotFound(err) {
			if err = e.restartContainer(c.ctx, item.ID(), 0, 0); err != nil {
				slog.Error("container restart after node recovery", "instance", item.ID(), "error", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		if err = c.watch(task); err != nil {
			return err
		}
	}
	return nil
}

func managedContainer(spec *oci.Spec, data, env, instance string) bool {
	for _, mount := range spec.Mounts {
		if mount.Destination == "/etc/resolv.conf" && mount.Source == filepath.Join(instanceDir(data, env, instance), "resolv.conf") {
			return true
		}
	}
	return false
}

func (e *Engine) restartContainer(ctx context.Context, instance string, pid, code uint32) (err error) {
	c := e.container
	container, err := c.client.LoadContainer(ctx, instance)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return err
	}
	unlocked := e.lock(labels[environmentLabel])
	defer unlocked()
	unlockedAsset := e.lock(labels[environmentLabel] + "/" + labels[assetLabel])
	defer unlockedAsset()
	labels, err = container.Labels(ctx)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if labels[desiredLabel] != "running" {
		return nil
	}
	var execution api.AssetExecution
	if err = json.Unmarshal([]byte(labels[executionLabel]), &execution); err != nil {
		return err
	}
	if !restartAfterExit(execution.Asset.RestartPolicy, code) {
		return nil
	}
	task, err := container.Task(ctx, nil)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if err == nil {
		status, err := task.Status(ctx)
		if err != nil {
			return err
		}
		if task.Pid() != pid || status.Status != containerd.Stopped {
			return nil
		}
	}
	select {
	case e.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.slots }()
	defer func() {
		if err != nil {
			_, saveErr := container.SetLabels(context.WithoutCancel(ctx), map[string]string{restartErrorLabel: err.Error()})
			err = errors.Join(err, saveErr)
		}
	}()
	if _, err = c.start(ctx, container, labels[environmentLabel], execution); err != nil {
		return err
	}
	var policies []api.Policy
	if labels[policiesLabel] != "" {
		if err = json.Unmarshal([]byte(labels[policiesLabel]), &policies); err != nil {
			return err
		}
	}
	return e.shape(ctx, labels[environmentLabel], execution, policiesByNetwork(api.EnvironmentSpec{Policies: &policies}))
}

func restartAfterExit(policy *api.RestartPolicy, code uint32) bool {
	return policy != nil && (*policy == api.Always || (*policy == api.OnFailure && code != 0))
}

func (c *Containers) savePolicies(ctx context.Context, instance string, policies *[]api.Policy) error {
	container, err := c.client.LoadContainer(ctx, instance)
	if err != nil {
		return err
	}
	value := []api.Policy{}
	if policies != nil {
		value = *policies
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = container.SetLabels(ctx, map[string]string{policiesLabel: string(raw)})
	return err
}
