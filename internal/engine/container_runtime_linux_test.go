//go:build linux

package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"netlab.local/core/api"
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
	archive := nonRootContainerArchive(t, ctx, data)
	e, err := New(ctx, Config{ID: uuid.NewString(), DataDir: data, ContainerdSocket: "/run/containerd/containerd.sock", OVNEndpoint: "unix:/run/ovn/ovnnb_db.sock", OVSEndpoint: "unix:/run/openvswitch/db.sock", Bridge: "br-int"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.slots = make(chan struct{}, 1)
	id := uuid.NewString()
	resources := api.Resources{Cpu: 0.25, MemoryMiB: 64, DiskGiB: 1}
	template := api.Template{Id: uuid.NewString(), Name: "non-root", Kind: api.Container, Source: archive, Version: 1, Resources: resources}
	asset := api.Asset{Id: "server", Name: "Server", TemplateId: template.Id, Resources: resources, Parameters: &map[string]string{"OVERRIDE_ME": "asset", "NETLAB_TEST_STOP_DELAY": "2s"}, Interfaces: []api.Interface{{Id: "nic", NetworkId: "lan", Address: "192.0.2.10", Mac: "02:ee:00:00:02:10", Primary: true}}}
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
	}
	if after, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err != nil || !bytes.Equal(hostPorts, after) {
		t.Fatalf("container network policy changed the host: before=%q after=%q error=%v", hostPorts, after, err)
	}
}

func nonRootContainerArchive(t *testing.T, ctx context.Context, directory string) string {
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
	config, err := json.Marshal(imagespec.Image{Platform: imagespec.Platform{Architecture: runtime.GOARCH, OS: "linux"}, Config: imagespec.ImageConfig{User: "1000:1000", Entrypoint: []string{"/server"}, Cmd: []string{"image-argument"}, WorkingDir: "/work", Env: []string{"IMAGE_DEFAULT=present", "OVERRIDE_ME=image"}}, RootFS: imagespec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(layer.Bytes())}}})
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
