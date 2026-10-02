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
	"reflect"
	"testing"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cio"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
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
	logicalSwitchIDs := func() string {
		t.Helper()
		output, err := exec.Command("ovn-nbctl", "--db=unix:/run/ovn/ovnnb_db.sock", "--columns=_uuid", "--format=csv", "--data=bare", "--no-headings", "find", "Logical_Switch", "external_ids:netlab.environment="+env).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(output)
	}
	beforePolicies := logicalSwitchIDs()
	delay := 8
	policies := []api.Policy{{Id: "slow-lan", NetworkId: "lan", Direction: api.Both, Action: api.Shape, DelayMs: &delay}}
	plan.Spec.Policies = &policies
	apply(api.NodePlanPhasePolicies, "running")
	apply(api.NodePlanPhasePolicies, "running")
	for _, asset := range plan.Assets {
		devices, err := e.devices(ctx, env, asset)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range devices {
			device, err := netlink.LinkByName(name)
			if err != nil {
				t.Fatal(err)
			}
			qdiscs, err := netlink.QdiscList(device)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, qdisc := range qdiscs {
				found = found || qdisc.Type() == "netem"
			}
			if !found {
				t.Fatalf("policies batch did not apply netem to %s", name)
			}
		}
	}
	if beforePolicies != logicalSwitchIDs() {
		t.Fatal("node policies batch rewrote the owner's OVN network")
	}
	plan.Spec.Policies = nil
	apply(api.NodePlanPhasePolicies, "running")
	// Verify the image entrypoint is serving HTTP inside its already-configured
	// business network, rather than only treating container task state as readiness.
	container, err := e.container.client.LoadContainer(ctx, clientExecution.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	checkContainerHTTP(t, ctx, container, "192.168.82.10:80")
	apply(api.NodePlanPhaseSuspend, "suspended")
	apply(api.NodePlanPhaseResume, "running")
	execContainer(t, ctx, container, "printf writable >/netlab-marker; printf durable >/usr/share/nginx/html/data-marker")
	updated := assets[2]
	updated.Asset.Resources.Cpu = 2
	updated.Asset.Resources.MemoryMiB = 192
	if state, err := e.container.Execute(ctx, env, api.NodePlanPhaseUpdate, updated); err != nil || state != "running" {
		t.Fatalf("live resources update: %s %v", state, err)
	}
	spec, err := container.Spec(ctx)
	if err != nil || *spec.Linux.Resources.Memory.Limit != 192<<20 || *spec.Linux.Resources.CPU.Quota != 200000 {
		t.Fatalf("resource configuration was not updated: %v", err)
	}
	observed, err := e.container.observedExecution(ctx, container)
	if err != nil || observed.Asset.Resources.Cpu != 2 || observed.Asset.Resources.MemoryMiB != 192 {
		t.Fatalf("live cgroup limits were not observed: %+v %v", observed, err)
	}
	before, err := container.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldPort := plan.Assets[2].Interfaces[0].PortName
	plan.Assets[2].Interfaces = append([]api.ResolvedInterface(nil), plan.Assets[2].Interfaces...)
	plan.Assets[2].Interfaces[0].PortName = uuid.NewString()
	apply(api.NodePlanPhaseForceStop, "stopped")
	if output, err := exec.Command("ovs-vsctl", "--columns=name", "--format=csv", "--data=bare", "--no-headings", "find", "Interface", "external_ids:iface-id="+oldPort).Output(); err != nil || len(output) > 0 {
		t.Fatalf("stop left the installed interface behind: %s %v", output, err)
	}
	retained := true
	addedVolumes := append(append([]api.Volume{}, volumes...), api.Volume{Id: "temporary", MountPath: "/netlab-temporary", SizeGiB: 1}, api.Volume{Id: "retained", MountPath: "/netlab-retained", SizeGiB: 1, Retain: &retained})
	plan.Assets[2].Asset.Volumes = &addedVolumes
	for i := range plan.Assets {
		plan.Assets[i].Asset.Resources.Cpu = 2
		plan.Assets[i].Asset.Resources.MemoryMiB += 64
		plan.Assets[i].Interfaces[0].Address = fmt.Sprintf("192.168.82.%d", 30+i)
		plan.Spec.Assets[i].Interfaces[0].Address = plan.Assets[i].Interfaces[0].Address
	}
	apply(api.NodePlanPhaseNetwork, "")
	apply(api.NodePlanPhaseUpdate, "stopped")
	apply(api.NodePlanPhaseStart, "running")
	container, err = e.container.client.LoadContainer(ctx, clientExecution.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	after, err := container.Info(ctx)
	if err != nil || before.SnapshotKey != after.SnapshotKey {
		t.Fatalf("update replaced the writable layer: %v", err)
	}
	execContainer(t, ctx, container, "test $(cat /netlab-marker) = writable && test $(cat /usr/share/nginx/html/data-marker) = durable && ip -4 addr show eth0 | grep -q 192.168.82.32")
	execContainer(t, ctx, container, "printf temporary >/netlab-temporary/marker; printf retained >/netlab-retained/marker")
	client := plan.Assets[2]
	if _, err = e.container.Execute(ctx, env, api.NodePlanPhaseForceStop, client); err != nil {
		t.Fatal(err)
	}
	client.Asset.Volumes = &volumes
	if _, err = e.container.Execute(ctx, env, api.NodePlanPhaseUpdate, client); err != nil {
		t.Fatal(err)
	}
	temporaryPath := e.container.volumeDir(env, client.Asset.Id, "temporary")
	if _, err = os.Stat(filepath.Join(temporaryPath, "marker")); err != nil {
		t.Fatal("update deleted an unmounted volume before commit", err)
	}
	cleanup := client
	removedVolumes := addedVolumes[1:]
	cleanup.Asset.Volumes = &removedVolumes
	if _, err = e.container.Execute(ctx, env, api.NodePlanPhaseCleanupVolumes, cleanup); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(temporaryPath); !os.IsNotExist(err) {
		t.Fatal("post-commit cleanup did not remove the temporary volume", err)
	}
	if _, err = os.Stat(filepath.Join(e.container.volumeDir(env, client.Asset.Id, "retained"), "marker")); err != nil {
		t.Fatal("post-commit cleanup removed a retained volume", err)
	}
	plan.Assets[2] = client
	replacement := client
	replacement.InstanceId = uuid.NewString()
	defer e.container.Execute(context.Background(), env, api.NodePlanPhaseDestroy, replacement)
	if _, err = e.container.Execute(ctx, env, api.NodePlanPhasePrepare, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = e.container.Execute(ctx, env, api.NodePlanPhaseActivate, replacement); err != nil {
		t.Fatal(err)
	}
	webContainer, err := e.container.client.LoadContainer(ctx, plan.Assets[0].InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.shape(ctx, env, replacement, map[string][]api.Policy{"lan": policies}); err != nil {
		t.Fatal(err)
	}
	checkContainerHTTP(t, ctx, webContainer, "192.168.82.32:80")
	t.Log("replacement HTTP responds with shaping before old instance cleanup")
	if detached, err := e.container.ovs.Detach(ctx, deviceName(replacement.Interfaces[0].PortName), env, "another-asset", replacement.InstanceId); err == nil || detached {
		t.Fatal("port cleanup crossed the asset ownership boundary")
	}
	if _, err = e.container.Execute(ctx, env, api.NodePlanPhaseDestroy, client); err != nil {
		t.Fatal(err)
	}
	device, err := netlink.LinkByName(deviceName(replacement.Interfaces[0].PortName))
	if err != nil {
		t.Fatal("old instance cleanup deleted the replacement interface", err)
	}
	qdiscs, err := netlink.QdiscList(device)
	if err != nil {
		t.Fatal(err)
	}
	shaped, redirected := false, false
	for _, qdisc := range qdiscs {
		if netem, ok := qdisc.(*netlink.Netem); ok {
			shaped = shaped || netem.Latency > 0
		}
		redirected = redirected || qdisc.Type() == "ingress"
	}
	if !shaped || !redirected {
		t.Fatal("old instance cleanup cleared the replacement traffic policy")
	}
	checkContainerHTTP(t, ctx, webContainer, "192.168.82.32:80")
	if _, err = os.Stat(filepath.Join(e.container.volumeDir(env, client.Asset.Id, "web-content"), "data-marker")); err != nil {
		t.Fatal("destroying the old instance removed the replacement volume", err)
	}
	newContainer, err := e.container.client.LoadContainer(ctx, replacement.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	execContainer(t, ctx, newContainer, "test $(cat /usr/share/nginx/html/data-marker) = durable")
	stale := replacement
	stale.Interfaces = nil
	stale.Asset.Volumes = nil
	if _, err = e.container.Execute(ctx, env, api.NodePlanPhaseDestroy, stale); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("ovs-vsctl", "--columns=name", "--format=csv", "--data=bare", "--no-headings", "find", "Interface", "external_ids:iface-id="+replacement.Interfaces[0].PortName).Output(); err != nil || len(output) > 0 {
		t.Fatalf("destroy left the installed interface behind: %s %v", output, err)
	}
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
	volumes := []api.Volume{{Id: "data", MountPath: "/data", SizeGiB: 1}}
	a := api.AssetExecution{Asset: api.Asset{Id: "modern", Name: "UEFI", Resources: api.Resources{Cpu: 1, MemoryMiB: 512, DiskGiB: 1}, Volumes: &volumes}, InstanceId: uuid.NewString(), Template: api.Template{Id: "modern", Source: base, Hardware: &api.Hardware{Firmware: api.Uefi, Machine: "pc-q35-8.2", DiskBus: api.HardwareDiskBusSata, NicModel: api.HardwareNicModelE1000e, SecureBoot: &yes, Tpm: &yes}}}
	a.Template.Kind = api.Vm
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
	domain, err := vm.conn.LookupDomainByUUIDString(a.InstanceId)
	if err != nil {
		t.Fatal(err)
	}
	defer domain.Free()
	readConfig := func() libvirtxml.Domain {
		t.Helper()
		text, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
		if err != nil {
			t.Fatal(err)
		}
		var config libvirtxml.Domain
		if err = config.Unmarshal(text); err != nil {
			t.Fatal(err)
		}
		return config
	}
	before := readConfig()
	a.Asset.Resources = api.Resources{Cpu: 2, MemoryMiB: 640, DiskGiB: 2}
	updatedVolumes := []api.Volume{{Id: "data", MountPath: "/data", SizeGiB: 2}, {Id: "temporary", MountPath: "/temporary", SizeGiB: 1}}
	a.Asset.Volumes = &updatedVolumes
	if state, err := vm.Execute(ctx, env, api.NodePlanPhaseUpdate, a); err != nil || state != "stopped" {
		t.Fatalf("firmware VM update: %s %v", state, err)
	}
	after := readConfig()
	if before.UUID != after.UUID || !reflect.DeepEqual(before.GenID, after.GenID) || !reflect.DeepEqual(before.OS.NVRam, after.OS.NVRam) || !reflect.DeepEqual(before.Devices.TPMs, after.Devices.TPMs) || before.Devices.Disks[0].Source.File.File != after.Devices.Disks[0].Source.File.File {
		t.Fatal("update changed instance, disk or firmware state identity")
	}
	if after.VCPU.Value != 2 || after.Memory.Value != 640*1024 || after.Memory.Unit != "KiB" {
		t.Fatalf("updated VM resources do not match requested configuration: CPU=%d memory=%d %s", after.VCPU.Value, after.Memory.Value, after.Memory.Unit)
	}
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseStart, a); err != nil {
		t.Fatal(err)
	}
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseForceStop, a); err != nil {
		t.Fatal(err)
	}
	if image, err := inspectImage(ctx, vm.volumePath(env, a.Asset.Id, "data")); err != nil || image.VirtualSize != 2<<30 {
		t.Fatalf("data disk expansion: %+v %v", image, err)
	}
	updatedVolumes = updatedVolumes[:1]
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseUpdate, a); err != nil {
		t.Fatal(err)
	}
	cleanup := a
	removed := []api.Volume{{Id: "temporary", SizeGiB: 1}}
	cleanup.Asset.Volumes = &removed
	if _, err = os.Stat(vm.volumePath(env, a.Asset.Id, "temporary")); err != nil {
		t.Fatal("VM update deleted a data disk before commit", err)
	}
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseCleanupVolumes, cleanup); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(vm.volumePath(env, a.Asset.Id, "temporary")); !os.IsNotExist(err) {
		t.Fatal("VM post-commit cleanup left an unused data disk", err)
	}
	failed := a
	badHardware := *a.Template.Hardware
	badHardware.Machine = "unsupported-machine"
	failed.Template.Hardware = &badHardware
	failed.Asset.Resources = api.Resources{Cpu: 3, MemoryMiB: 700, DiskGiB: 3}
	executor := &Engine{vm: vm, slots: make(chan struct{}, 1), locks: make(map[string]*objectLock)}
	result := executor.Execute(ctx, api.NodePlan{OperationId: uuid.NewString(), EnvironmentId: env, Phase: api.NodePlanPhaseUpdate, Assets: []api.AssetExecution{failed}})
	actual := result.Results[0]
	if actual.Error != nil {
		t.Logf("native failure: %s", *actual.Error)
	}
	if actual.Error == nil || actual.Execution == nil || actual.State != "stopped" {
		t.Fatalf("native update failure was not returned with actual configuration: %+v", actual)
	}
	resources := actual.Execution.Asset.Resources
	if resources.Cpu != 2 || resources.MemoryMiB != 640 || resources.DiskGiB != 3 || actual.Execution.Template.Hardware.Machine == badHardware.Machine {
		t.Fatalf("failed update echoed requested configuration: %+v", actual.Execution)
	}
	a.Asset.Resources.DiskGiB = resources.DiskGiB
	replacement := a
	replacement.InstanceId = uuid.NewString()
	defer vm.Execute(context.Background(), env, api.NodePlanPhaseDestroy, replacement)
	if _, err = vm.Execute(ctx, env, api.NodePlanPhasePrepare, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseDestroy, a); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(vm.volumePath(env, a.Asset.Id, "data")); err != nil {
		t.Fatal("destroying the old VM removed the replacement data disk", err)
	}
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseStart, replacement); err != nil {
		t.Fatal(err)
	}
	stale := replacement
	stale.Asset.Volumes = nil
	if _, err = vm.Execute(ctx, env, api.NodePlanPhaseDestroy, stale); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(vm.volumePath(env, a.Asset.Id, "data")); !os.IsNotExist(err) {
		t.Fatal("VM destroy left its installed data disk behind", err)
	}
}

func checkContainerHTTP(t *testing.T, ctx context.Context, container containerd.Container, address string) {
	t.Helper()
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
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp4", address)
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
		return fmt.Errorf("container service %s not responding", address)
	}); err != nil {
		t.Fatal(err)
	}
}

func execContainer(t *testing.T, ctx context.Context, container containerd.Container, script string) {
	t.Helper()
	task, err := container.Task(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	processSpec := *spec.Process
	processSpec.Args = []string{"/bin/sh", "-c", script}
	process, err := task.Exec(ctx, uuid.NewString(), &processSpec, cio.NullIO)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Delete(context.Background())
	status, err := process.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-status:
		code, _, err := exit.Result()
		if err != nil || code != 0 {
			t.Fatalf("container command exit=%d error=%v: %s", code, err, script)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
