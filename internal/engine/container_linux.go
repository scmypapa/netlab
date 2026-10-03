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
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/images/archive"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/sys/signal"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

const environmentLabel = "netlab.environment"
const assetLabel = "netlab.asset"
const networkLabel = "netlab.interfaces"
const executionLabel = "netlab.execution"

type Containers struct {
	client   *containerd.Client
	data     string
	ovs      *network.OVS
	images   sync.Map
	ctx      context.Context
	cancel   context.CancelFunc
	watching sync.Map
	watches  sync.WaitGroup
	restart  func(context.Context, string, uint32, uint32) error
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
	ctx, cancel := context.WithCancel(ctx)
	return &Containers{client: c, data: data, ovs: ovs, ctx: namespaces.WithNamespace(ctx, "netlab"), cancel: cancel}, nil
}
func (c *Containers) Close() { c.cancel(); c.watches.Wait(); c.client.Close() }
func (c *Containers) Execute(ctx context.Context, env string, phase api.NodePlanPhase, a api.AssetExecution) (string, error) {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	if phase == api.NodePlanPhasePrepare {
		return c.prepare(ctx, env, a)
	}
	container, err := c.client.LoadContainer(ctx, a.InstanceId)
	if errdefs.IsNotFound(err) {
		if phase == api.NodePlanPhaseDestroy {
			return "destroyed", c.removeFiles(ctx, env, a)
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
	case api.NodePlanPhaseCleanupVolumes:
		references, err := c.volumeReferences(ctx, env, a.Asset.Id)
		if err != nil {
			return state, err
		}
		return state, removeVolumeFiles(a.Asset.Volumes, references, func(id string) string { return c.volumeDir(env, a.Asset.Id, id) }, true)
	case api.NodePlanPhaseUpdate:
		return c.update(ctx, container, env, a)
	case api.NodePlanPhaseStart:
		if state == "stopped" || state == "prepared" || state == "created" {
			return c.start(ctx, container, env, a)
		}
	case api.NodePlanPhaseStop, api.NodePlanPhaseForceStop:
		if _, err = container.SetLabels(ctx, map[string]string{desiredLabel: "stopped"}); err != nil {
			return state, err
		}
		err = c.stop(ctx, container, a, phase == api.NodePlanPhaseForceStop)
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
		if _, err = container.SetLabels(ctx, map[string]string{desiredLabel: "stopped"}); err != nil {
			return state, err
		}
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
		return "destroyed", c.removeFiles(ctx, env, a)
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
	image, err := c.image(ctx, a.Template, nil)
	if err != nil {
		return "absent", err
	}
	config, err := imageConfig(ctx, image)
	if err != nil {
		return "absent", err
	}
	dir := instanceDir(c.data, env, a.InstanceId)
	if err = os.MkdirAll(dir, 0711); err != nil {
		return "absent", err
	}
	if err = writeNetworkFiles(dir, a.Interfaces); err != nil {
		return "absent", err
	}
	volumes := managedVolumes(config, a.Asset.Volumes)
	mounts := []specs.Mount{{Destination: "/etc/resolv.conf", Type: "bind", Source: filepath.Join(dir, "resolv.conf"), Options: []string{"rbind", "ro"}}, {Destination: "/etc/hosts", Type: "bind", Source: filepath.Join(dir, "hosts"), Options: []string{"rbind", "ro"}}}
	volumeMounts, err := c.volumeMounts(env, a.Asset.Id, volumes)
	if err != nil {
		return "absent", err
	}
	mounts = append(mounts, volumeMounts...)
	networkJSON, err := json.Marshal(a.Interfaces)
	if err != nil {
		return "absent", err
	}
	volumeJSON, err := json.Marshal(volumes)
	if err != nil {
		return "absent", err
	}
	executionJSON, err := json.Marshal(a)
	if err != nil {
		return "absent", err
	}
	labels := map[string]string{environmentLabel: env, assetLabel: a.Asset.Id, networkLabel: string(networkJSON), executionLabel: string(executionJSON), desiredLabel: "stopped", "netlab.volumes": string(volumeJSON), "netlab.stop-signal": config.Config.StopSignal}
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
func (c *Containers) image(ctx context.Context, t api.Template, registry *api.RegistryCredentials) (containerd.Image, error) {
	ref := fmt.Sprintf("netlab/template/%s:%d", t.Id, t.Version)
	lock, _ := c.images.LoadOrStore(ref, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	image, err := c.client.GetImage(ctx, ref)
	if errdefs.IsNotFound(err) {
		image, err = c.importImage(ctx, t, registry)
	}
	if err != nil {
		return nil, err
	}
	if err = image.Unpack(ctx, "overlayfs"); err != nil {
		return nil, err
	}
	return image, nil
}

func (c *Containers) prepareTemplate(ctx context.Context, t api.Template, registry *api.RegistryCredentials) (api.Template, error) {
	directory := templateDirectory(c.data, t.Id, t.Version)
	if raw, err := os.ReadFile(filepath.Join(directory, "template.json")); err == nil {
		origin := t.ArtifactNodeId
		err = json.Unmarshal(raw, &t)
		t.ArtifactNodeId = origin
		return t, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return t, err
	}
	source := t
	source.ArtifactNodeId = nil
	image, err := c.image(ctx, source, registry)
	if err != nil {
		return t, err
	}
	if err = os.MkdirAll(filepath.Dir(directory), 0711); err != nil {
		return t, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(directory), "import-")
	if err != nil {
		return t, err
	}
	defer os.RemoveAll(staging)
	if err = os.Chmod(staging, 0711); err != nil {
		return t, err
	}
	file, err := os.OpenFile(filepath.Join(staging, "image.tar"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return t, err
	}
	exportErr := c.client.Export(ctx, file, archive.WithManifest(image.Target(), image.Name()), archive.WithPlatform(platforms.Default()), archive.WithSkipMissing(c.client.ContentStore()))
	if err = errors.Join(exportErr, file.Close()); err != nil {
		return t, err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return t, err
	}
	if err = os.WriteFile(filepath.Join(staging, "template.json"), raw, 0640); err != nil {
		return t, err
	}
	return t, os.Rename(staging, directory)
}

func imageConfig(ctx context.Context, image containerd.Image) (imagespec.Image, error) {
	var config imagespec.Image
	descriptor, err := image.Config(ctx)
	if err != nil {
		return config, err
	}
	blob, err := content.ReadBlob(ctx, image.ContentStore(), descriptor)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(blob, &config)
	return config, err
}

func managedVolumes(config imagespec.Image, configured *[]api.Volume) []api.Volume {
	volumes := []api.Volume{}
	if configured != nil {
		volumes = append(volumes, (*configured)...)
	}
	declared := make(map[string]bool)
	for _, v := range volumes {
		declared[v.MountPath] = true
	}
	paths := []string{}
	for path := range config.Config.Volumes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	// Stable image-volume identities do not change when another mount is overridden.
	for i, path := range paths {
		if !declared[path] {
			volumes = append(volumes, api.Volume{Id: fmt.Sprintf("image-volume-%d", i), MountPath: path, SizeGiB: 1})
		}
	}
	return volumes
}

func (c *Containers) volumeMounts(env, asset string, volumes []api.Volume) ([]specs.Mount, error) {
	mounts := make([]specs.Mount, 0, len(volumes))
	for _, volume := range volumes {
		if !filepath.IsAbs(volume.MountPath) || filepath.Clean(volume.MountPath) == "/" {
			return nil, fmt.Errorf("invalid container volume mount path %s", volume.MountPath)
		}
		mounts = append(mounts, specs.Mount{Destination: volume.MountPath, Type: "bind", Source: c.volumeDir(env, asset, volume.Id), Options: []string{"rbind", "rw"}})
	}
	return mounts, nil
}
func (c *Containers) importImage(ctx context.Context, t api.Template, registry *api.RegistryCredentials) (containerd.Image, error) {
	ctx, release, err := c.client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer release(ctx)
	source := t.Source
	name := fmt.Sprintf("netlab/template/%s:%d", t.Id, t.Version)
	if t.ArtifactNodeId != nil {
		source = filepath.Join(templateDirectory(c.data, t.Id, t.Version), "image.tar")
	}
	if strings.HasPrefix(source, "/") || strings.HasPrefix(source, "file:") || strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		f, err := openArtifact(ctx, source)
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
		return c.adoptImage(ctx, name, containerd.NewImage(c.client, items[0]))
	}
	ref, err := reference.ParseNormalizedNamed(t.Source)
	if err != nil {
		return nil, errors.New("invalid registry image reference")
	}
	registryRef := reference.TagNameOnly(ref).String()
	host := reference.Domain(ref)
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	authorizer := docker.NewDockerAuthorizer(docker.WithAuthCreds(func(requestHost string) (string, string, error) {
		if registry == nil || requestHost != host {
			return "", "", nil
		}
		username, password := "", ""
		if registry.Username != nil {
			username = *registry.Username
		}
		if registry.Password != nil {
			password = *registry.Password
		}
		return username, password, nil
	}))
	hosts := docker.ConfigureDefaultRegistries(docker.WithAuthorizer(authorizer), docker.WithPlainHTTP(func(requestHost string) (bool, error) {
		return registry != nil && registry.PlainHttp != nil && *registry.PlainHttp && requestHost == reference.Domain(ref), nil
	}))
	image, err := c.client.Pull(ctx, registryRef, containerd.WithResolver(docker.NewResolver(docker.ResolverOptions{Hosts: hosts})))
	if err != nil {
		return nil, err
	}
	return c.adoptImage(ctx, name, image)
}

func (c *Containers) adoptImage(ctx context.Context, name string, image containerd.Image) (containerd.Image, error) {
	if image.Name() == name {
		return image, nil
	}
	if _, err := c.client.ImageService().Create(ctx, images.Image{Name: name, Target: image.Target()}); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(image.Name(), "netlab/template/") {
		if err := c.client.ImageService().Delete(ctx, image.Name()); err != nil && !errdefs.IsNotFound(err) {
			return nil, errors.Join(err, c.client.ImageService().Delete(ctx, name))
		}
	}
	return c.client.GetImage(ctx, name)
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
	if _, err := container.SetLabels(ctx, map[string]string{desiredLabel: "running", restartErrorLabel: ""}); err != nil {
		return "unknown", err
	}
	if task, err := container.Task(ctx, nil); err == nil {
		status, err := task.Status(ctx)
		if err != nil {
			return "unknown", err
		}
		if status.Status == containerd.Stopped {
			if err = c.disconnect(ctx, env, a); err != nil {
				return "stopped", err
			}
			if _, err = task.Delete(ctx); err != nil {
				return "stopped", err
			}
		} else if status.Status == containerd.Created {
			if err = c.connect(ctx, task.Pid(), env, a); err != nil {
				return "created", err
			}
			if err = c.watch(task); err != nil {
				return "created", err
			}
			if err = task.Start(ctx); err != nil {
				return "created", err
			}
			return containerState(ctx, container)
		} else {
			return containerState(ctx, container)
		}
	} else if !errdefs.IsNotFound(err) {
		return "unknown", err
	}
	dir := instanceDir(c.data, env, a.InstanceId)
	task, err := container.NewTask(ctx, containerLogFiles(dir))
	if err != nil {
		return "stopped", err
	}
	// NewTask is the OCI created state. The entrypoint cannot run until all
	// business interfaces, addresses, routes and DNS are ready.
	if err = c.connect(ctx, task.Pid(), env, a); err != nil {
		cleanup := errors.Join(c.disconnect(context.WithoutCancel(ctx), env, a), deleteCreated(context.WithoutCancel(ctx), task))
		return "stopped", errors.Join(err, cleanup)
	}
	if err = c.watch(task); err != nil {
		return "created", err
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
	labels, err := container.Labels(ctx)
	if err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(labels[networkLabel]), &a.Interfaces); err != nil {
		return err
	}
	task, err := container.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return c.disconnect(ctx, labels[environmentLabel], a)
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
	return c.disconnect(ctx, labels[environmentLabel], a)
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
		labels, spec, err := containerMetadata(ctx, item)
		if err != nil {
			return results, err
		}
		if labels[assetLabel] == "" {
			continue
		}
		if !managedContainer(spec, c.data, labels[environmentLabel], item.ID()) {
			continue
		}
		state, err := containerState(ctx, item)
		if labels[restartErrorLabel] != "" {
			err = errors.Join(err, errors.New(labels[restartErrorLabel]))
		}
		r := api.ExecutionResult{EnvironmentId: ptr(labels[environmentLabel]), AssetId: labels[assetLabel], InstanceId: item.ID(), State: state, ObservedAt: time.Now().UTC()}
		var observeErr error
		r.Execution, observeErr = c.observedExecution(ctx, item)
		err = errors.Join(err, observeErr)
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
func (c *Containers) removeFiles(ctx context.Context, env string, a api.AssetExecution) error {
	if err := os.RemoveAll(instanceDir(c.data, env, a.InstanceId)); err != nil {
		return err
	}
	references, err := c.volumeReferences(ctx, env, a.Asset.Id)
	if err != nil {
		return err
	}
	return removeVolumeFiles(a.Asset.Volumes, references, func(id string) string { return c.volumeDir(env, a.Asset.Id, id) }, false)
}

func (c *Containers) volumeReferences(ctx context.Context, env, asset string) (map[string]bool, error) {
	references := make(map[string]bool)
	users, err := c.client.Containers(ctx, "labels.\""+environmentLabel+"\"=="+strconvQuote(env)+",labels.\""+assetLabel+"\"=="+strconvQuote(asset))
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		spec, err := user.Spec(ctx)
		if err != nil {
			return nil, err
		}
		for _, mount := range spec.Mounts {
			references[mount.Source] = true
		}
	}
	return references, nil
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
