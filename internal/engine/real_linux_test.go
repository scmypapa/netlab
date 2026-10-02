//go:build linux

package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/uuid"
	"netlab.local/core/api"
)

// NETLAB_REAL_IMAGE points to a Docker/OCI archive. The test uses the same engine
// that serves node plans and always removes only its newly allocated environment.
func TestRealMixedLifecycle(t *testing.T) {
	archive := os.Getenv("NETLAB_REAL_IMAGE")
	if archive == "" {
		t.Skip("set NETLAB_REAL_IMAGE to run against local libvirt, containerd and OVN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	data, err := os.MkdirTemp("/var/lib", "netlab-real-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	if err = os.Chmod(data, 0711); err != nil {
		t.Fatal(err)
	}
	env := uuid.NewString()
	e, err := New(ctx, Config{ID: uuid.NewString(), Name: "execution-test", DataDir: data, ContainerdSocket: "/run/containerd/containerd.sock", LibvirtURI: "qemu:///system", OVNEndpoint: "unix:/run/ovn/ovnnb_db.sock", OVSEndpoint: "unix:/run/openvswitch/db.sock", Bridge: "br-int"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	base := filepath.Join(data, "base.qcow2")
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", base, "1G"); err != nil {
		t.Fatal(err)
	}
	containerResources := api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}
	vmResources := api.Resources{Cpu: 1, MemoryMiB: 256, DiskGiB: 1}
	volumes := []api.Volume{{Id: "web-content", MountPath: "/usr/share/nginx/html", SizeGiB: 1}}
	interfaces := func(id, address, mac string) []api.Interface {
		return []api.Interface{{Id: id, NetworkId: "lan", Mac: mac, Address: address, Primary: true}}
	}
	resolve := func(i api.Interface) []api.ResolvedInterface {
		return []api.ResolvedInterface{{Id: i.Id, NetworkId: i.NetworkId, Mac: i.Mac, Address: i.Address, Prefix: 24, Primary: true, PortName: uuid.NewString(), Mtu: 1400}}
	}
	web := api.Asset{Id: "web", Name: "Web", TemplateId: "nginx", Resources: containerResources, Interfaces: interfaces("web-nic", "192.168.82.10", "02:00:00:82:00:10"), Volumes: &volumes}
	vm := api.Asset{Id: "vm", Name: "VM", TemplateId: "bios", Resources: vmResources, Interfaces: interfaces("vm-nic", "192.168.82.20", "02:00:00:82:00:20")}
	assets := []api.AssetExecution{{Asset: web, Template: api.Template{Id: "nginx", Name: "Nginx", Kind: api.Container, Source: archive, Resources: containerResources}, InstanceId: uuid.NewString(), Interfaces: resolve(web.Interfaces[0])}, {Asset: vm, Template: api.Template{Id: "bios", Name: "BIOS", Kind: api.Vm, Source: base, Os: "linux", Resources: vmResources, Hardware: &api.Hardware{Firmware: api.Bios, Machine: "pc-i440fx-8.2", DiskBus: api.HardwareDiskBusIde, NicModel: api.HardwareNicModelE1000}}, InstanceId: uuid.NewString(), Interfaces: resolve(vm.Interfaces[0])}}
	clientAsset := web
	clientAsset.Id = "client"
	clientAsset.Name = "Client"
	clientAsset.Interfaces = interfaces("client-nic", "192.168.82.11", "02:00:00:82:00:11")
	clientExecution := assets[0]
	clientExecution.Asset = clientAsset
	clientExecution.InstanceId = uuid.NewString()
	clientExecution.Interfaces = resolve(clientAsset.Interfaces[0])
	assets = append(assets, clientExecution)
	plan := api.NodePlan{OperationId: uuid.NewString(), EnvironmentId: env, Assets: assets, Spec: api.EnvironmentSpec{Assets: []api.Asset{web, vm}, Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "192.168.82.0/24"}}}}
	plan.Spec.Assets = append(plan.Spec.Assets, clientAsset)
	defer func() {
		cleanup := plan
		cleanup.Phase = api.NodePlanPhaseDestroy
		r := e.Execute(context.Background(), cleanup)
		for _, r := range r.Results {
			if r.Error != nil {
				t.Errorf("cleanup: %s", *r.Error)
			}
		}
		cleanup.Phase = api.NodePlanPhaseRemoveNetwork
		if r = e.Execute(context.Background(), cleanup); r.Error != nil {
			t.Errorf("network cleanup: %s", *r.Error)
		}
	}()
	apply := func(phase api.NodePlanPhase, want string) {
		t.Helper()
		plan.Phase = phase
		r := e.Execute(ctx, plan)
		if r.Error != nil {
			t.Fatalf("%s: %s", phase, *r.Error)
		}
		for _, v := range r.Results {
			if v.Error != nil {
				t.Fatalf("%s %s: %s", phase, v.AssetId, *v.Error)
			}
			if want != "" && v.State != want {
				t.Fatalf("%s %s: %s expected %s", phase, v.AssetId, v.State, want)
			}
		}
		t.Logf("%s completed", phase)
	}
	apply(api.NodePlanPhaseNetwork, "")
	apply(api.NodePlanPhasePrepare, "")
	apply(api.NodePlanPhaseActivate, "running")
	// Verify the image entrypoint is serving HTTP inside its already-configured
	// business network, rather than only treating container task state as readiness.
	container, err := e.container.client.LoadContainer(ctx, clientExecution.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := ns.GetNS(fmt.Sprintf("/proc/%d/ns/net", task.Pid()))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err = handle.Do(func(_ ns.NetNS) error {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", "192.168.82.10:80")
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: web\r\nConnection: close\r\n\r\n")
			if err != nil {
				conn.Close()
				return err
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err == nil {
				resp.Body.Close()
				conn.Close()
				if resp.StatusCode == 200 {
					return nil
				}
				return fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			conn.Close()
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("container service not responding")
	}); err != nil {
		t.Fatal(err)
	}
	apply(api.NodePlanPhaseSuspend, "suspended")
	apply(api.NodePlanPhaseResume, "running")
	apply(api.NodePlanPhaseForceStop, "stopped")
	apply(api.NodePlanPhaseStart, "running")
	apply(api.NodePlanPhaseDestroy, "destroyed")
	apply(api.NodePlanPhaseRemoveNetwork, "")
	inventory, err := e.Inventory(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Results) != 0 {
		t.Fatal("managed instances remain after destroy")
	}
	if output, err := exec.Command("ovn-nbctl", "--db=unix:/run/ovn/ovnnb_db.sock", "--format=csv", "--data=bare", "--no-headings", "find", "Logical_Switch", "external_ids:netlab.environment="+env).Output(); err != nil || len(output) > 0 {
		t.Fatalf("logical switches remain: %s %v", output, err)
	}
}

func TestRealUEFISecureBootTPM(t *testing.T) {
	if os.Getenv("NETLAB_REAL_IMAGE") == "" {
		t.Skip("set NETLAB_REAL_IMAGE to enable local execution tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data, err := os.MkdirTemp("/var/lib", "netlab-firmware-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	if err = os.Chmod(data, 0711); err != nil {
		t.Fatal(err)
	}
	vm, err := NewVirtualMachines("qemu:///system", data, "br-int")
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	base := filepath.Join(data, "base.qcow2")
	if err = command(ctx, "qemu-img", "create", "-f", "qcow2", base, "1G"); err != nil {
		t.Fatal(err)
	}
	yes := true
	env := uuid.NewString()
	a := api.AssetExecution{Asset: api.Asset{Id: "modern", Name: "UEFI", Resources: api.Resources{Cpu: 1, MemoryMiB: 512, DiskGiB: 1}}, InstanceId: uuid.NewString(), Template: api.Template{Id: "modern", Source: base, Hardware: &api.Hardware{Firmware: api.Uefi, Machine: "pc-q35-8.2", DiskBus: api.HardwareDiskBusSata, NicModel: api.HardwareNicModelE1000e, SecureBoot: &yes, Tpm: &yes}}}
	defer func() {
		if _, err := vm.Execute(context.Background(), env, api.NodePlanPhaseDestroy, a); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	for _, phase := range []api.NodePlanPhase{api.NodePlanPhasePrepare, api.NodePlanPhaseActivate, api.NodePlanPhaseSuspend, api.NodePlanPhaseResume, api.NodePlanPhaseForceStop} {
		state, err := vm.Execute(ctx, env, phase, a)
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		t.Logf("%s: %s", phase, state)
	}
	if _, err = os.Stat(filepath.Join(instanceDir(data, env, a.InstanceId), "nvram.fd")); err != nil {
		t.Fatal("per-instance NVRAM was not created", err)
	}
}
