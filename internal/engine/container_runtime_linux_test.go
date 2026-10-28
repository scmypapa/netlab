//go:build linux

package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"netlab.local/core/api"
	"netlab.local/core/internal/environment"
)

func TestContainerNetworkFilesIgnoreServiceUmask(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "netlab-network-files-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	for range 2 {
		if err := writeNetworkFiles(directory, []api.ResolvedInterface{{Dns: &[]string{"192.0.2.53"}}}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"hosts", "resolv.conf"} {
			path := filepath.Join(directory, name)
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0644 {
				t.Fatalf("%s is not readable by the image user: %v %v", name, info, err)
			}
			if err = os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRealNonRootContainerRuntime(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CONTAINER") == "" {
		t.Skip("set NETLAB_REAL_CONTAINER to exercise local containerd and OVN with a non-root image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	hostPorts, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.MkdirTemp("/tmp", "netlab-nonroot-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	archive := nonRootContainerArchive(t, ctx, data, "/work")
	e, err := New(ctx, Config{ID: uuid.NewString(), DataDir: data, ContainerdSocket: "/run/containerd/containerd.sock", OVNEndpoint: "unix:/run/ovn/ovnnb_db.sock", OVSEndpoint: "unix:/run/openvswitch/db.sock", Bridge: "br-int"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	plugins, err := e.container.client.IntrospectionService().Plugins(ctx, []string{"type==io.containerd.snapshotter.v1,id==overlayfs"})
	if err != nil || len(plugins.Plugins) != 1 {
		t.Fatalf("read actual snapshotter: %v %v", plugins, err)
	}
	root := plugins.Plugins[0].Exports["root"]
	actual, err := StorageInfo(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := e.Info(ctx)
	if err != nil || info.ContainerStorage == nil || info.ContainerStorage.Path != root || info.ContainerStorage.Filesystem != actual.Filesystem || info.ContainerStorage.CapacityBytes != actual.CapacityBytes || info.ContainerStorage.AvailableBytes <= 0 || info.ContainerStorage.Path == data {
		t.Fatalf("node reported a guessed container storage root: %+v %v", info.ContainerStorage, err)
	}
	e.slots = make(chan struct{}, 1)
	id := uuid.NewString()
	resources := api.Resources{Cpu: 0.25, MemoryMiB: 64, DiskGiB: 1}
	template := api.Template{Id: uuid.NewString(), Name: "non-root", Kind: api.Container, Source: archive, Version: 1, Resources: resources}
	template, err = e.PrepareTemplate(ctx, api.NodeTemplatePreparation{Template: template})
	if err != nil {
		t.Fatal(err)
	}
	asset := api.Asset{Id: "server", Name: "Server", TemplateId: template.Id, Resources: resources, Parameters: &map[string]string{"OVERRIDE_ME": "asset", "NETLAB_TEST_STOP_DELAY": "2s"}, Interfaces: []api.Interface{{Id: "nic", NetworkId: "lan", Address: "192.0.2.10", Mac: "02:ee:00:00:02:10", Primary: true}}}
	asset.Volumes = environment.ResolveVolumeDefaults(template, nil)
	asset.RestartPolicy = ptr(api.Always)
	resolved := api.ResolvedInterface{Id: "nic", NetworkId: "lan", PortName: uuid.NewString(), Address: "192.0.2.10", Prefix: 24, Mac: "02:ee:00:00:02:10", Mtu: 1400, Primary: true, Dns: &[]string{"192.0.2.53"}}
	a := api.AssetExecution{Asset: asset, Template: template, InstanceId: uuid.NewString(), Interfaces: []api.ResolvedInterface{resolved}}
	plan := api.NodePlan{EnvironmentId: id, OperationId: uuid.NewString(), Assets: []api.AssetExecution{a}, Spec: api.EnvironmentSpec{Assets: []api.Asset{asset}, Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.0.2.0/24"}}}}
	defer func() {
		for _, phase := range []api.NodePlanPhase{api.NodePlanPhaseDestroy, api.NodePlanPhaseRemoveNetwork} {
			plan.Phase = phase
			result := e.Execute(context.Background(), plan)
			if result.Error != nil {
				t.Errorf("cleanup %s: %s", phase, *result.Error)
			}
			for _, result := range result.Results {
				if result.Error != nil {
					t.Errorf("cleanup %s: %s", phase, *result.Error)
				}
			}
		}
		if err := e.RemoveTemplate(context.Background(), template.Id, nil); err != nil {
			t.Error(err)
		}
	}()
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	for _, phase := range []api.NodePlanPhase{api.NodePlanPhaseNetwork, api.NodePlanPhasePrepare, api.NodePlanPhaseStart, api.NodePlanPhaseStop, api.NodePlanPhaseStart} {
		plan.Phase = phase
		var result api.NodeResult
		if phase == api.NodePlanPhaseStop {
			stopped := make(chan api.NodeResult, 1)
			go func() { stopped <- e.Execute(ctx, plan) }()
			container, err := e.container.client.LoadContainer(ctx, a.InstanceId)
			if err != nil {
				t.Fatal(err)
			}
			waiting, finish := context.WithTimeout(ctx, time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			for {
				labels, err := container.Labels(waiting)
				if err != nil {
					t.Fatal(err)
				}
				if labels[desiredLabel] == "stopped" && len(e.slots) == 0 {
					break
				}
				select {
				case <-waiting.Done():
					t.Fatal("guest stop wait retained the host compute slot")
				case <-ticker.C:
				}
			}
			ticker.Stop()
			finish()
			other := plan
			other.EnvironmentId, other.Phase = uuid.NewString(), api.NodePlanPhaseInspect
			other.Assets = []api.AssetExecution{a}
			other.Assets[0].InstanceId = uuid.NewString()
			control, finish := context.WithTimeout(ctx, 500*time.Millisecond)
			observed := e.Execute(control, other)
			finish()
			if len(observed.Results) != 1 || observed.Results[0].State != "absent" {
				t.Fatalf("other environment could not enter compute while guest stopped: %+v", observed)
			}
			for _, key := range []string{id, id + "/" + asset.Id} {
				locked, finish := context.WithTimeout(ctx, 50*time.Millisecond)
				unlock, err := e.lock(locked, key)
				finish()
				if unlock != nil {
					unlock()
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("guest stop wait released its object lock %s: %v", key, err)
				}
			}
			select {
			case result = <-stopped:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		} else {
			result = e.Execute(ctx, plan)
		}
		if result.Error != nil {
			t.Fatalf("%s: %s", phase, *result.Error)
		}
		for _, result := range result.Results {
			if result.Error != nil {
				t.Fatalf("%s: %s", phase, *result.Error)
			}
			want := map[api.NodePlanPhase]string{api.NodePlanPhasePrepare: "prepared", api.NodePlanPhaseStart: "running", api.NodePlanPhaseStop: "stopped"}[phase]
			if result.State != want {
				t.Fatalf("%s state=%s, want=%s", phase, result.State, want)
			}
		}
		if phase == api.NodePlanPhaseStart {
			container, err := e.container.client.LoadContainer(ctx, a.InstanceId)
			if err != nil {
				t.Fatal(err)
			}
			checkContainerHTTP(t, ctx, container, "192.0.2.10:502")
		}
		if phase == api.NodePlanPhaseStop {
			for _, name := range []string{"hosts", "resolv.conf"} {
				if err := os.Chmod(filepath.Join(assetDirectory(data, id, a), name), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if after, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err != nil || !bytes.Equal(hostPorts, after) {
		t.Fatalf("container network policy changed the host: before=%q after=%q error=%v", hostPorts, after, err)
	}
	t.Run("volume observation and live resource update", func(t *testing.T) {
		container, err := e.container.client.LoadContainer(ctx, a.InstanceId)
		if err != nil {
			t.Fatal(err)
		}
		legacy := a
		legacy.Asset.Volumes = nil
		raw, err := json.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = container.SetLabels(ctx, map[string]string{executionLabel: string(raw)}); err != nil {
			t.Fatal(err)
		}
		observed, err := e.container.observedExecution(ctx, container)
		if err != nil {
			t.Fatal(err)
		}
		if observed.Asset.Volumes == nil || len(*observed.Asset.Volumes) != 1 || (*observed.Asset.Volumes)[0].Id != "image-volume-0" {
			t.Fatalf("runtime observation lost the actual volume: %+v", observed.Asset.Volumes)
		}
		a.Asset.Resources.Cpu = 0.5
		if state, err := e.container.Execute(ctx, id, api.NodePlanPhaseUpdate, a, nil); err != nil || state != "running" {
			t.Fatalf("live update changed the image mount: %s %v", state, err)
		}
		checkContainerHTTP(t, ctx, container, "192.0.2.10:502")
	})
	t.Run("automatic restart retains its running intent", func(t *testing.T) {
		container, err := e.container.client.LoadContainer(ctx, a.InstanceId)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			unlock, err := e.lock(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			task, err := container.Task(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			exited, err := task.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = task.Kill(ctx, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			select {
			case <-exited:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if state, err := containerState(ctx, container); err != nil || state != "restarting" {
				t.Fatalf("restart intent was reported as released compute: %s %v", state, err)
			}
		}()
		plan.Phase = api.NodePlanPhaseStart
		result := e.Execute(ctx, plan)
		if result.Error != nil || len(result.Results) != 1 || result.Results[0].Error != nil || result.Results[0].State != "running" {
			t.Fatalf("restart could not complete: %+v", result)
		}
		checkContainerHTTP(t, ctx, container, "192.0.2.10:502")
	})
	t.Run("normal stop finishes when PID 1 cannot handle the stop signal", func(t *testing.T) {
		container, err := e.container.client.LoadContainer(ctx, a.InstanceId)
		if err != nil {
			t.Fatal(err)
		}
		task, err := container.Task(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = task.Kill(ctx, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		stopping, finish := context.WithTimeout(ctx, 20*time.Second)
		defer finish()
		plan.Phase = api.NodePlanPhaseStop
		result := e.Execute(stopping, plan)
		if result.Error != nil || len(result.Results) != 1 || result.Results[0].Error != nil || result.Results[0].State != "stopped" {
			if err := task.Kill(context.WithoutCancel(ctx), syscall.SIGCONT); err != nil && !errdefs.IsNotFound(err) {
				t.Error(err)
			}
			t.Fatalf("unresponsive PID 1 prevented normal stop: %+v", result)
		}
		if _, err = container.Task(ctx, nil); !errdefs.IsNotFound(err) {
			t.Fatalf("stopped task was not removed: %v", err)
		}
		plan.Phase = api.NodePlanPhaseStart
		result = e.Execute(ctx, plan)
		if result.Error != nil || len(result.Results) != 1 || result.Results[0].Error != nil || result.Results[0].State != "running" {
			t.Fatalf("stopped container could not restart: %+v", result)
		}
		checkContainerHTTP(t, ctx, container, "192.0.2.10:502")
	})
	t.Run("same-node migration keeps the surviving instance and volume", func(t *testing.T) {
		if state, err := e.container.Execute(ctx, id, api.NodePlanPhaseStop, a, nil); err != nil || state != "stopped" {
			t.Fatalf("stop source: %s %v", state, err)
		}
		target := a
		target.InstanceId = uuid.NewString()
		targetPath := filepath.Join(data, "destination")
		target.StoragePath = &targetPath
		if state, err := e.container.Execute(ctx, id, api.NodePlanPhasePrepare, target, nil); err != nil || state != "prepared" {
			t.Fatalf("prepare destination: %s %v", state, err)
		}
		t.Cleanup(func() {
			if _, err := e.container.Execute(context.WithoutCancel(ctx), id, api.NodePlanPhaseDestroy, target, nil); err != nil {
				t.Error(err)
			}
		})
		volume := api.NodeVolume{Id: uuid.NewString(), Kind: api.Container, StoragePoolId: "data", Storage: api.StorageInfo{Path: data}}
		path := persistentDirectory(volume)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(path, "survivor")
		if err := os.WriteFile(marker, []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		source := a
		source.VolumeSources = map[string]api.NodeVolume{"data": volume}
		target.VolumeSources = source.VolumeSources
		raw, err := json.Marshal(api.NodeMigrationCleanup{Source: source, Target: target, TargetNodeId: e.cfg.ID})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodDelete, "/migration", bytes.NewReader(raw)).WithContext(ctx)
		request.SetPathValue("environmentId", id)
		request.SetPathValue("assetId", a.Asset.Id)
		request.SetPathValue("instanceId", a.InstanceId)
		response := httptest.NewRecorder()
		e.Migration(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("cleanup rejected a different target instance: %d %s", response.Code, response.Body.String())
		}
		if _, err := e.container.client.LoadContainer(ctx, source.InstanceId); !errdefs.IsNotFound(err) {
			t.Fatalf("source remains after cleanup: %v", err)
		}
		if state, err := e.container.Execute(ctx, id, api.NodePlanPhaseInspect, target, nil); err != nil || state != "stopped" {
			t.Fatalf("surviving destination was removed: %s %v", state, err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("retained local volume was removed", err)
		}
		if err := e.cleanupMigration(ctx, id, source, target, "other-node"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("same path on another node incorrectly retained the local copy", err)
		}
	})
}

func nonRootContainerArchive(t *testing.T, ctx context.Context, directory string, volumePaths ...string) string {
	t.Helper()
	binary := filepath.Join(directory, "server")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/fixtures/container-runtime.go")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build image fixture: %s %v", output, err)
	}
	program, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	var layer bytes.Buffer
	writer := tar.NewWriter(&layer)
	for _, header := range []tar.Header{{Name: "work", Mode: 0755, Typeflag: tar.TypeDir}, {Name: "etc", Mode: 0755, Typeflag: tar.TypeDir}, {Name: "server", Mode: 0755, Size: int64(len(program))}} {
		if err = writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Name == "server" {
			if _, err = writer.Write(program); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	volumes := map[string]struct{}{}
	for _, path := range volumePaths {
		volumes[path] = struct{}{}
	}
	config, err := json.Marshal(imagespec.Image{Platform: imagespec.Platform{Architecture: runtime.GOARCH, OS: "linux"}, Config: imagespec.ImageConfig{User: "1000:1000", Entrypoint: []string{"/server"}, Cmd: []string{"image-argument"}, WorkingDir: "/work", Env: []string{"IMAGE_DEFAULT=present", "OVERRIDE_ME=image"}, Volumes: volumes}, RootFS: imagespec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(layer.Bytes())}}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal([]map[string]any{{"Config": "config.json", "RepoTags": []string{"netlab-test/nonroot:" + uuid.NewString()}, "Layers": []string{"layer.tar"}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "image.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer = tar.NewWriter(file)
	for name, data := range map[string][]byte{"manifest.json": manifest, "config.json": config, "layer.tar": layer.Bytes()} {
		if err = writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
