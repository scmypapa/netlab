//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/platforms"
	"github.com/moby/sys/signal"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

const environmentLabel = "netlab.environment"
const assetLabel = "netlab.asset"
const networkLabel = "netlab.interfaces"

type Containers struct {
	client *containerd.Client
	data   string
	ovs    *network.OVS
	images sync.Map
}

func NewContainers(ctx context.Context, socket, data string, ovs *network.OVS) (*Containers, error) {
	c, err := containerd.New(socket, containerd.WithDefaultNamespace("netlab"))
	if err != nil {
		return nil, err
	}
	if _, err = c.Version(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return &Containers{client: c, data: data, ovs: ovs}, nil
}
func (c *Containers) Close() { c.client.Close() }
func (c *Containers) Execute(ctx context.Context, env string, phase api.NodePlanPhase, a api.AssetExecution) (string, error) {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	if phase == api.NodePlanPhasePrepare {
		return c.prepare(ctx, env, a)
	}
	container, err := c.client.LoadContainer(ctx, a.InstanceId)
	if errdefs.IsNotFound(err) {
		if phase == api.NodePlanPhaseDestroy {
			return "destroyed", c.removeFiles(env, a)
		}
		return "absent", err
	}
	if err != nil {
		return "unknown", err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return "unknown", err
	}
	if labels[environmentLabel] != env || labels[assetLabel] != a.Asset.Id {
		return "unknown", errors.New("container ownership does not match plan")
	}
	state, err := containerState(ctx, container)
	if err != nil {
		return state, err
	}
	switch phase {
	case api.NodePlanPhaseActivate, api.NodePlanPhaseStart:
		if state == "stopped" || state == "prepared" || state == "created" {
			return c.start(ctx, container, env, a)
		}
	case api.NodePlanPhaseStop, api.NodePlanPhaseForceStop:
		if state == "running" || state == "suspended" || state == "created" {
			err = c.stop(ctx, container, a, phase == api.NodePlanPhaseForceStop)
		}
	case api.NodePlanPhaseReboot:
		if err = c.stop(ctx, container, a, false); err == nil {
			return c.start(ctx, container, env, a)
		}
	case api.NodePlanPhaseSuspend, api.NodePlanPhaseResume:
		var t containerd.Task
		t, err = container.Task(ctx, nil)
		if err == nil {
			if phase == api.NodePlanPhaseSuspend && state != "suspended" {
				err = t.Pause(ctx)
			} else if phase == api.NodePlanPhaseResume && state == "suspended" {
				err = t.Resume(ctx)
			}
		}
	case api.NodePlanPhaseDestroy:
		if err = c.stop(ctx, container, a, true); err != nil {
			return state, err
		}
		var volumes []api.Volume
		if err = json.Unmarshal([]byte(labels["netlab.volumes"]), &volumes); err != nil {
			return "stopped", err
		}
		a.Asset.Volumes = &volumes
		if err = container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			return "stopped", err
		}
		return "destroyed", c.removeFiles(env, a)
	case api.NodePlanPhaseInspect:
	default:
		return state, fmt.Errorf("invalid container phase %s", phase)
	}
	if err != nil {
		return state, err
	}
	return containerState(ctx, container)
}
func (c *Containers) prepare(ctx context.Context, env string, a api.AssetExecution) (string, error) {
	if old, err := c.client.LoadContainer(ctx, a.InstanceId); err == nil {
		labels, err := old.Labels(ctx)
		if err != nil {
			return "unknown", err
		}
		if labels[environmentLabel] != env || labels[assetLabel] != a.Asset.Id {
			return "unknown", errors.New("container ownership does not match plan")
		}
		return containerState(ctx, old)
	} else if !errdefs.IsNotFound(err) {
		return "unknown", err
	}
	image, err := c.image(ctx, a.Template)
	if err != nil {
		return "absent", err
	}
	ic, err := image.Config(ctx)
	if err != nil {
		return "absent", err
	}
	blob, err := content.ReadBlob(ctx, image.ContentStore(), ic)
	if err != nil {
		return "absent", err
	}
	var config imagespec.Image
	if err = json.Unmarshal(blob, &config); err != nil {
		return "absent", err
	}
	dir := instanceDir(c.data, env, a.InstanceId)
	if err = os.MkdirAll(dir, 0711); err != nil {
		return "absent", err
	}
	if err = writeNetworkFiles(dir, a.Interfaces); err != nil {
		return "absent", err
	}
	volumes := []api.Volume{}
	if a.Asset.Volumes != nil {
		volumes = append(volumes, (*a.Asset.Volumes)...)
	}
	// Docker VOLUME declarations become managed volumes instead of hiding image data.
	declared := make(map[string]bool)
	for _, v := range volumes {
		declared[v.MountPath] = true
	}
	paths := []string{}
	for path := range config.Config.Volumes {
		if !declared[path] {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for i, path := range paths {
		volumes = append(volumes, api.Volume{Id: fmt.Sprintf("image-volume-%d", i), MountPath: path, SizeGiB: 1})
	}
	mounts := []specs.Mount{{Destination: "/etc/resolv.conf", Type: "bind", Source: filepath.Join(dir, "resolv.conf"), Options: []string{"rbind", "ro"}}, {Destination: "/etc/hosts", Type: "bind", Source: filepath.Join(dir, "hosts"), Options: []string{"rbind", "ro"}}}
	for _, vol := range volumes {
		if !filepath.IsAbs(vol.MountPath) || filepath.Clean(vol.MountPath) == "/" {
			return "absent", fmt.Errorf("invalid container volume mount path %s", vol.MountPath)
		}
		mounts = append(mounts, specs.Mount{Destination: vol.MountPath, Type: "bind", Source: c.volumeDir(env, a.Asset.Id, vol.Id), Options: []string{"rbind", "rw"}})
	}
	networkJSON, err := json.Marshal(a.Interfaces)
	if err != nil {
		return "absent", err
	}
	volumeJSON, err := json.Marshal(volumes)
	if err != nil {
		return "absent", err
	}
	labels := map[string]string{environmentLabel: env, assetLabel: a.Asset.Id, networkLabel: string(networkJSON), "netlab.volumes": string(volumeJSON), "netlab.stop-signal": config.Config.StopSignal}
	opts := []oci.SpecOpts{oci.WithImageConfig(image), oci.WithHostname(a.Asset.Name), oci.WithMemoryLimit(uint64(a.Asset.Resources.MemoryMiB) << 20), oci.WithMounts(mounts), cpuLimit(a.Asset.Resources.Cpu)}
	if a.Asset.Parameters != nil {
		envs := []string{}
		for k, v := range *a.Asset.Parameters {
			envs = append(envs, k+"="+v)
		}
		sort.Strings(envs)
		opts = append(opts, oci.WithEnv(envs))
	}
	created, err := c.client.NewContainer(ctx, a.InstanceId, containerd.WithImage(image), containerd.WithSnapshotter("overlayfs"), containerd.WithNewSnapshot(a.InstanceId, image), containerd.WithContainerLabels(labels), containerd.WithNewSpec(opts...))
	if err != nil {
		return "absent", err
	}
	// Initialize before mounting, so the first writable volume contains the image's data.
	if err = c.initializeVolumes(ctx, created, env, a.Asset.Id, volumes); err != nil {
		return "absent", errors.Join(err, created.Delete(context.WithoutCancel(ctx), containerd.WithSnapshotCleanup))
	}
	return "prepared", nil
}
func (c *Containers) image(ctx context.Context, t api.Template) (containerd.Image, error) {
	ref := fmt.Sprintf("netlab/template/%s:%d", t.Id, t.Version)
	lock, _ := c.images.LoadOrStore(ref, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if image, err := c.client.GetImage(ctx, ref); err == nil {
		return image, nil
	} else if !errdefs.IsNotFound(err) {
		return nil, err
	}
	image, err := c.importImage(ctx, t)
	if err != nil {
		return nil, err
	}
	if _, err = c.client.ImageService().Create(ctx, images.Image{Name: ref, Target: image.Target()}); err != nil {
		return nil, err
	}
	return c.client.GetImage(ctx, ref)
}
func (c *Containers) importImage(ctx context.Context, t api.Template) (containerd.Image, error) {
	if strings.HasPrefix(t.Source, "/") || strings.HasPrefix(t.Source, "file:") {
		f, err := os.Open(strings.TrimPrefix(t.Source, "file://"))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		items, err := c.client.Import(ctx, f, containerd.WithImportPlatform(platforms.Default()), containerd.WithSkipMissing())
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, errors.New("OCI archive contains no images")
		}
		img, err := c.client.GetImage(ctx, items[0].Name)
		if err != nil {
			return nil, err
		}
		if err = img.Unpack(ctx, "overlayfs"); err != nil {
			return nil, err
		}
		return img, nil
	}
	if img, err := c.client.GetImage(ctx, t.Source); err == nil {
		if err = img.Unpack(ctx, "overlayfs"); err != nil {
			return nil, err
		}
		return img, nil
	} else if !errdefs.IsNotFound(err) {
		return nil, err
	}
	return c.client.Pull(ctx, t.Source, containerd.WithPullUnpack)
}
func cpuLimit(cpus int) oci.SpecOpts {
	return func(ctx context.Context, client oci.Client, c *containers.Container, s *oci.Spec) error {
		if s.Linux.Resources == nil {
			s.Linux.Resources = &specs.LinuxResources{}
		}
		if s.Linux.Resources.CPU == nil {
			s.Linux.Resources.CPU = &specs.LinuxCPU{}
		}
		period := uint64(100000)
		quota := int64(cpus) * int64(period)
		s.Linux.Resources.CPU.Period = &period
		s.Linux.Resources.CPU.Quota = &quota
		return nil
	}
}
func (c *Containers) initializeVolumes(ctx context.Context, container containerd.Container, env, asset string, volumes []api.Volume) error {
	if len(volumes) == 0 {
		return nil
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
		for _, v := range volumes {
			dst := c.volumeDir(env, asset, v.Id)
			if _, err := os.Stat(dst); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err = os.MkdirAll(filepath.Dir(dst), 0750); err != nil {
				return err
			}
			resolved, err := fs.RootPath(root, v.MountPath)
			if err != nil {
				return err
			}
			stage, err := os.MkdirTemp(filepath.Dir(dst), "initializing-")
			if err != nil {
				return err
			}
			if _, err = os.Stat(resolved); err == nil {
				err = fs.CopyDir(stage, resolved)
			} else if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err == nil {
				err = os.Rename(stage, dst)
			}
			os.RemoveAll(stage)
			if err != nil {
				return err
			}
		}
		return nil
	})
}
func (c *Containers) start(ctx context.Context, container containerd.Container, env string, a api.AssetExecution) (string, error) {
	if task, err := container.Task(ctx, nil); err == nil {
		status, err := task.Status(ctx)
		if err != nil {
			return "unknown", err
		}
		if status.Status == containerd.Stopped {
			if err = c.disconnect(ctx, a); err != nil {
				return "stopped", err
			}
			if _, err = task.Delete(ctx); err != nil {
				return "stopped", err
			}
		} else if status.Status == containerd.Created {
			if err = c.connect(ctx, task.Pid(), env, a); err != nil {
				return "created", err
			}
			if err = task.Start(ctx); err != nil {
				return "created", err
			}
			return containerState(ctx, container)
		}
	} else if !errdefs.IsNotFound(err) {
		return "unknown", err
	}
	dir := instanceDir(c.data, env, a.InstanceId)
	task, err := container.NewTask(ctx, cio.LogFile(filepath.Join(dir, "stdout.log")))
	if err != nil {
		return "stopped", err
	}
	// NewTask is the OCI created state. The entrypoint cannot run until all
	// business interfaces, addresses, routes and DNS are ready.
	if err = c.connect(ctx, task.Pid(), env, a); err != nil {
		cleanup := errors.Join(c.disconnect(context.WithoutCancel(ctx), a), deleteCreated(context.WithoutCancel(ctx), task))
		return "stopped", errors.Join(err, cleanup)
	}
	if err = task.Start(ctx); err != nil {
		return "created", err
	}
	return containerState(ctx, container)
}
func deleteCreated(ctx context.Context, task containerd.Task) error {
	_, err := task.Delete(ctx, containerd.WithProcessKill)
	return err
}
func (c *Containers) stop(ctx context.Context, container containerd.Container, a api.AssetExecution, force bool) error {
	task, err := container.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return c.disconnect(ctx, a)
	}
	if err != nil {
		return err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return err
	}
	if status.Status != containerd.Stopped {
		if status.Status == containerd.Paused {
			if err = task.Resume(ctx); err != nil {
				return err
			}
		}
		wait, err := task.Wait(ctx)
		if err != nil {
			return err
		}
		stopSignal := syscall.SIGTERM
		if force {
			stopSignal = syscall.SIGKILL
		} else {
			labels, err := container.Labels(ctx)
			if err != nil {
				return err
			}
			if labels["netlab.stop-signal"] != "" {
				stopSignal, err = signal.ParseSignal(labels["netlab.stop-signal"])
				if err != nil {
					return err
				}
			}
		}
		if err = task.Kill(ctx, stopSignal); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		timer := time.NewTimer(90 * time.Second)
		defer timer.Stop()
		select {
		case exit := <-wait:
			if _, _, err = exit.Result(); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("container has not completed normal stop")
		}
	}
	if _, err = task.Delete(ctx); err != nil {
		return err
	}
	return c.disconnect(ctx, a)
}
func containerState(ctx context.Context, c containerd.Container) (string, error) {
	t, err := c.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return "stopped", nil
	}
	if err != nil {
		return "unknown", err
	}
	s, err := t.Status(ctx)
	if err != nil {
		return "unknown", err
	}
	switch s.Status {
	case containerd.Running:
		return "running", nil
	case containerd.Paused:
		return "suspended", nil
	case containerd.Stopped:
		return "stopped", nil
	case containerd.Created:
		return "created", nil
	default:
		return "unknown", nil
	}
}
func (c *Containers) Inventory(ctx context.Context, env string) ([]api.ExecutionResult, error) {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	filter := "labels.\"" + assetLabel + "\""
	if env != "" {
		filter = "labels.\"" + environmentLabel + "\"==" + strconvQuote(env)
	}
	items, err := c.client.Containers(ctx, filter)
	if err != nil {
		return nil, err
	}
	results := []api.ExecutionResult{}
	for _, item := range items {
		labels, err := item.Labels(ctx)
		if err != nil {
			return results, err
		}
		if labels[assetLabel] == "" {
			continue
		}
		state, err := containerState(ctx, item)
		r := api.ExecutionResult{AssetId: labels[assetLabel], InstanceId: item.ID(), State: state, ObservedAt: time.Now().UTC()}
		if err != nil {
			r.Error = ptr(err.Error())
		}
		results = append(results, r)
	}
	return results, nil
}
func (c *Containers) volumeDir(env, asset, volume string) string {
	return filepath.Join(c.data, "environments", env, "volumes", asset, volume)
}
func (c *Containers) removeFiles(env string, a api.AssetExecution) error {
	if err := os.RemoveAll(instanceDir(c.data, env, a.InstanceId)); err != nil {
		return err
	}
	if a.Asset.Volumes != nil {
		for _, v := range *a.Asset.Volumes {
			if v.Retain == nil || !*v.Retain {
				if err := os.RemoveAll(c.volumeDir(env, a.Asset.Id, v.Id)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
