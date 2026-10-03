//go:build linux

package engine

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func TestRealVMArtifactImports(t *testing.T) {
	if os.Getenv("NETLAB_REAL_VM_IMPORTS") == "" {
		t.Skip("set NETLAB_REAL_VM_IMPORTS to test actual qemu-img and libvirt imports")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	data, err := os.MkdirTemp("/var/lib", "netlab-import-")
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
	sources := filepath.Join(data, "fixtures")
	if err = os.MkdirAll(sources, 0711); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct{ name, size string }{{"boot.raw", "1G"}, {"data.raw", "2G"}} {
		path := filepath.Join(sources, fixture.name)
		if err = command(ctx, "qemu-img", "create", "-f", "raw", path, fixture.size); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteAt([]byte("netlab-import-content:"+fixture.name), 512)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, conversion := range []struct{ source, output, format string }{{"boot.raw", "boot.qcow2", "qcow2"}, {"boot.raw", "boot.vmdk", "vmdk"}, {"data.raw", "data.vmdk", "vmdk"}} {
		if err = command(ctx, "qemu-img", "convert", "-f", "raw", "-O", conversion.format, filepath.Join(sources, conversion.source), filepath.Join(sources, conversion.output)); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := `<?xml version="1.0"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1" xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData" xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData" xmlns:vmw="http://www.vmware.com/schema/ovf">
 <References><File ovf:id="boot-file" ovf:href="boot.vmdk"/><File ovf:id="data-file" ovf:href="data.vmdk"/></References>
 <DiskSection><Disk ovf:diskId="boot-disk" ovf:fileRef="boot-file" ovf:capacity="1" ovf:capacityAllocationUnits="byte * 2^30"/><Disk ovf:diskId="data-disk" ovf:fileRef="data-file" ovf:capacity="2" ovf:capacityAllocationUnits="byte * 2^30"/></DiskSection>
 <VirtualSystem ovf:id="guest"><OperatingSystemSection><Description>Linux</Description></OperatingSystemSection><VirtualHardwareSection>
  <System><vssd:VirtualSystemType>vmx-13</vssd:VirtualSystemType></System>
  <Item><rasd:InstanceID>1</rasd:InstanceID><rasd:ResourceType>3</rasd:ResourceType><rasd:VirtualQuantity>2</rasd:VirtualQuantity></Item>
  <Item><rasd:InstanceID>2</rasd:InstanceID><rasd:ResourceType>4</rasd:ResourceType><rasd:VirtualQuantity>192</rasd:VirtualQuantity><rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits></Item>
  <Item><rasd:InstanceID>3</rasd:InstanceID><rasd:ResourceType>6</rasd:ResourceType><rasd:ResourceSubType>lsilogic</rasd:ResourceSubType></Item>
  <Item><rasd:InstanceID>4</rasd:InstanceID><rasd:ResourceType>6</rasd:ResourceType><rasd:ResourceSubType>VirtualSCSI</rasd:ResourceSubType></Item>
  <Item><rasd:InstanceID>5</rasd:InstanceID><rasd:ResourceType>17</rasd:ResourceType><rasd:Parent>3</rasd:Parent><rasd:AddressOnParent>0</rasd:AddressOnParent><rasd:HostResource>ovf:/disk/boot-disk</rasd:HostResource></Item>
  <Item><rasd:InstanceID>6</rasd:InstanceID><rasd:ResourceType>17</rasd:ResourceType><rasd:Parent>4</rasd:Parent><rasd:AddressOnParent>0</rasd:AddressOnParent><rasd:HostResource>ovf:/disk/data-disk</rasd:HostResource></Item>
  <Item><rasd:InstanceID>7</rasd:InstanceID><rasd:ResourceType>10</rasd:ResourceType><rasd:ResourceSubType>VmxNet3</rasd:ResourceSubType></Item>
  <Item><rasd:InstanceID>8</rasd:InstanceID><rasd:ResourceType>10</rasd:ResourceType><rasd:ResourceSubType>E1000</rasd:ResourceSubType></Item>
  <vmw:Config vmw:key="firmware" vmw:value="bios"/>
 </VirtualHardwareSection></VirtualSystem>
</Envelope>`
	if err = os.WriteFile(filepath.Join(sources, "guest.ovf"), []byte(descriptor), 0640); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(sources, "guest.ova")
	output, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(output)
	for _, name := range []string{"guest.ovf", "boot.vmdk", "data.vmdk"} {
		file, err := os.Open(filepath.Join(sources, name))
		if err != nil {
			t.Fatal(err)
		}
		info, _ := file.Stat()
		if err = writer.WriteHeader(&tar.Header{Name: name, Mode: 0640, Size: info.Size()}); err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(writer, file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	output.Close()
	server := httptest.NewServer(http.FileServer(http.Dir(sources)))
	defer server.Close()
	for _, fixture := range []struct {
		name, source string
		format       api.TemplateFormat
		disks        int
	}{
		{"qcow2", filepath.Join(sources, "boot.qcow2"), api.Qcow2, 1},
		{"raw", filepath.Join(sources, "boot.raw"), api.Raw, 1},
		{"vmdk", filepath.Join(sources, "boot.vmdk"), api.Vmdk, 1},
		{"ovf", filepath.Join(sources, "guest.ovf"), api.Ovf, 2},
		{"ova", archive, api.Ova, 2},
		{"http-ovf", server.URL + "/guest.ovf", api.Ovf, 2},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			template := api.Template{Id: uuid.NewString(), Name: fixture.name, Kind: api.Vm, Os: "linux", Source: fixture.source, Format: &fixture.format, Version: 1, Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 88}}
			if fixture.disks == 1 {
				template.Hardware = &api.Hardware{Firmware: api.Bios, Machine: "pc", DiskBus: api.HardwareDiskBusIde, NicModel: api.HardwareNicModelE1000}
			}
			prepared, err := vm.prepareTemplate(ctx, template, nil)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Disks == nil || len(*prepared.Disks) != fixture.disks || prepared.Resources.DiskGiB != int64(2*fixture.disks-1) {
				t.Fatalf("imported disks or total capacity: %+v", prepared)
			}
			if fixture.disks == 2 && (prepared.Resources.Cpu != 2 || prepared.Resources.MemoryMiB != 192 || (*prepared.Disks)[0].ControllerModel == nil || *(*prepared.Disks)[0].ControllerModel != "lsilogic" || *(*prepared.Disks)[1].ControllerModel != "vmpvscsi") {
				t.Fatalf("OVF hardware was not preserved: %+v", prepared)
			}
			cache := filepath.Join(data, "artifacts", prepared.Id, "1")
			for index, source := range []string{"boot.raw", "data.raw"}[:fixture.disks] {
				if err = command(ctx, "qemu-img", "compare", "-f", "raw", "-F", "qcow2", filepath.Join(sources, source), systemDiskPath(cache, index)); err != nil {
					t.Fatal("conversion changed disk content:", err)
				}
			}
			before, _ := os.Stat(systemDiskPath(cache, 0))
			if _, err = vm.prepareTemplate(ctx, prepared, nil); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(systemDiskPath(cache, 0))
			if before.ModTime() != after.ModTime() {
				t.Fatal("cache hit converted the immutable template again")
			}
			env, instance := uuid.NewString(), uuid.NewString()
			volumes := []api.Volume{{Id: "scratch", MountPath: "/scratch", SizeGiB: 1}}
			a := api.AssetExecution{InstanceId: instance, Template: prepared, Asset: api.Asset{Id: "guest", Name: fixture.name, TemplateId: prepared.Id, Resources: prepared.Resources, Volumes: &volumes}}
			if fixture.disks == 2 {
				for index := range 2 {
					a.Interfaces = append(a.Interfaces, api.ResolvedInterface{Id: fmt.Sprint(index), Mac: fmt.Sprintf("02:ee:cc:12:00:%02x", index+1), PortName: uuid.NewString(), Mtu: 1400})
				}
			}
			defer func() {
				if _, err := vm.Execute(context.Background(), env, api.NodePlanPhaseDestroy, a); err != nil {
					t.Errorf("cleanup: %v", err)
				}
			}()
			for _, phase := range []api.NodePlanPhase{api.NodePlanPhasePrepare, api.NodePlanPhaseStart, api.NodePlanPhaseSuspend, api.NodePlanPhaseResume, api.NodePlanPhaseForceStop} {
				if _, err = vm.Execute(ctx, env, phase, a); err != nil {
					t.Fatalf("%s: %v", phase, err)
				}
			}
			domain, err := vm.conn.LookupDomainByUUIDString(instance)
			if err != nil {
				t.Fatal(err)
			}
			defer domain.Free()
			text, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
			if err != nil {
				t.Fatal(err)
			}
			var beforeXML libvirtxml.Domain
			if err = beforeXML.Unmarshal(text); err != nil {
				t.Fatal(err)
			}
			if len(beforeXML.Devices.Disks) != fixture.disks+1 {
				t.Fatal("VM definition lost a system disk or duplicated the ordinary volume")
			}
			a.Asset.Resources.DiskGiB++
			a.Asset.Resources.MemoryMiB += 64
			if _, err = vm.Execute(ctx, env, api.NodePlanPhaseUpdate, a); err != nil {
				t.Fatal(err)
			}
			actual, err := vm.observedExecution(domain)
			if err != nil || actual.Asset.Resources.DiskGiB != a.Asset.Resources.DiskGiB || actual.Asset.Resources.MemoryMiB != a.Asset.Resources.MemoryMiB {
				t.Fatalf("actual resources after update: %+v %v", actual, err)
			}
			for index, disk := range *actual.Template.Disks {
				want := (*prepared.Disks)[index].SizeGiB
				if index == 0 {
					want++
				}
				if disk.SizeGiB != want {
					t.Fatalf("disk %s expansion: got %d want %d", disk.Id, disk.SizeGiB, want)
				}
			}
			if fixture.disks == 2 && (actual.Template.NicModels == nil || (*actual.Template.NicModels)[0] != "vmxnet3" || (*actual.Template.NicModels)[1] != "e1000") {
				t.Fatal("different OVF NIC models were lost")
			}
			if _, err = vm.Execute(ctx, env, api.NodePlanPhaseStart, a); err != nil {
				t.Fatal("updated VM did not start:", err)
			}
			if _, err = vm.Execute(ctx, env, api.NodePlanPhaseDestroy, a); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(instanceDir(data, env, instance)); !os.IsNotExist(err) {
				t.Fatal("destroy left instance disks behind:", err)
			}
			if _, err = os.Stat(vm.volumePath(env, a, "scratch")); !os.IsNotExist(err) {
				t.Fatal("destroy left its ordinary data volume behind:", err)
			}
			t.Logf("%s: %d disks, original controllers/NICs, lossless conversion, lifecycle, expansion and destroy passed", fixture.name, fixture.disks)
		})
	}
}
