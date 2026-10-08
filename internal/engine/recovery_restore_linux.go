//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd"
	filesystem "github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	imagearchive "github.com/containerd/containerd/images/archive"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/namespaces"
	"github.com/google/uuid"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/types/known/anypb"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

func readRecoveryManifest(directory, env, point string, a api.AssetExecution) (recoveryManifest, error) {
	var manifest recoveryManifest
	raw, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return manifest, err
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return manifest, err
	}
	if manifest.EnvironmentID != env || manifest.PointID != point || manifest.Execution.InstanceId != a.InstanceId || manifest.Execution.Asset.Id != a.Asset.Id {
		return manifest, errors.New("recovery artifact ownership does not match request")
	}
	return manifest, nil
}

func (e *Engine) prepareRecovery(ctx context.Context, plan api.NodePlan, a api.AssetExecution) (err error) {
	source, ok := (*plan.RecoverySources)[a.Asset.Id]
	if !ok || source.Execution.Asset.Id != a.Asset.Id {
		return errors.New("recovery source does not match target")
	}
	move := a.InstanceId == source.Execution.InstanceId && a.DataSetId == source.Execution.DataSetId
	directory := assetDirectory(e.cfg.DataDir, plan.EnvironmentId, a)
	if _, err = readRecoveryManifest(directory, source.EnvironmentId, *plan.RecoveryPointId, source.Execution); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Preparation owns only this operation's new data, never the current instance data.
	if err = e.removeRecoveryData(ctx, plan.EnvironmentId, a, move); err != nil {
		return err
	}
	staging := directory + ".pending"
	if err = os.RemoveAll(staging); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(staging, "input"), 0711); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(staging))
		if err != nil {
			err = errors.Join(err, e.removeRecoveryData(context.WithoutCancel(ctx), plan.EnvironmentId, a, move))
		}
	}()
	input := filepath.Join(staging, "input")
	destinationPool := ""
	if a.Rbd != nil {
		destinationPool = *a.StoragePoolId
	}
	if source.NodeId == e.cfg.ID && source.Backup == nil {
		if err = e.copyRecovery(ctx, source.EnvironmentId, *plan.RecoveryPointId, source.Execution, input, destinationPool); err != nil {
			return err
		}
	} else {
		reader, _, sourceErr := e.openRecoverySource(ctx, *plan.RecoveryPointId, source, destinationPool)
		if sourceErr != nil {
			return sourceErr
		}
		defer reader.Close()
		if err = receiveDirectoryArtifact(reader, input); err != nil {
			return err
		}
	}
	manifest, err := readRecoveryManifest(input, source.EnvironmentId, *plan.RecoveryPointId, source.Execution)
	if err != nil {
		return err
	}
	templateDir := templateDirectory(e.cfg.DataDir, a.Template.Id, a.Template.Version)
	if a.Template.ArtifactNodeId != nil {
		endpoint := ""
		if plan.ArtifactEndpoints != nil {
			endpoint = (*plan.ArtifactEndpoints)[*a.Template.ArtifactNodeId]
		}
		runtimeOnly := a.Rbd != nil
		for _, disk := range manifest.Disks {
			if disk.BackingTemplateDisk != nil {
				runtimeOnly = false
			}
		}
		if err = e.fetchTemplateArtifact(ctx, a.Template, endpoint, runtimeOnly); err != nil {
			return err
		}
		if runtimeOnly {
			templateDir = templateRuntimeDirectory(e.cfg.DataDir, a.Template.Id, a.Template.Version)
		}
	}
	switch a.Template.Kind {
	case api.Container:
		err = e.container.prepareRecovery(ctx, plan.EnvironmentId, a, input, staging, manifest)
	case api.Vm:
		err = e.vm.prepareRecovery(ctx, plan.EnvironmentId, a, input, staging, templateDir, manifest)
	}
	if err != nil {
		return err
	}
	if err = os.Rename(filepath.Join(input, "manifest.json"), filepath.Join(staging, "manifest.json")); err != nil {
		return err
	}
	if err = os.RemoveAll(input); err != nil {
		return err
	}
	return os.Rename(staging, directory)
}

func writeRecoveryJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func readRecoveryContainer(path string) (containers.Container, error) {
	// containerd uses typeurl.Any interfaces; JSON needs their concrete protobuf type.
	info := containers.Container{Spec: &anypb.Any{}, Runtime: containers.RuntimeInfo{Options: &anypb.Any{}}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(raw, &info)
	return info, err
}

func recoveryLeaseID(a api.AssetExecution) string {
	return "recovery-" + a.InstanceId + "." + a.DataSetId
}

func (c *Containers) prepareRecovery(ctx context.Context, env string, a api.AssetExecution, input, staging string, manifest recoveryManifest) (err error) {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	if _, err = c.image(ctx, a.Template, nil); err != nil {
		return err
	}
	if _, err = c.client.LeasesService().Create(ctx, leases.WithID(recoveryLeaseID(a))); err != nil && !errdefs.IsAlreadyExists(err) {
		return err
	}
	ctx = leases.WithLease(ctx, recoveryLeaseID(a))
	file, err := os.Open(filepath.Join(input, "container.tar"))
	if err != nil {
		return err
	}
	archive, importErr := imagearchive.ImportIndex(ctx, c.client.ContentStore(), file)
	if err = errors.Join(importErr, file.Close()); err != nil {
		return err
	}
	raw, err := content.ReadBlob(ctx, c.client.ContentStore(), archive)
	if err != nil {
		return err
	}
	var exported imagespec.Index
	if err = json.Unmarshal(raw, &exported); err != nil {
		return err
	}
	if len(exported.Manifests) != 1 {
		return errors.New("recovery archive must contain one checkpoint")
	}
	raw, err = content.ReadBlob(ctx, c.client.ContentStore(), exported.Manifests[0])
	if err != nil {
		return err
	}
	var index imagespec.Index
	if err = json.Unmarshal(raw, &index); err != nil {
		return err
	}
	info := containers.Container{ID: a.InstanceId}
	key := a.InstanceId + "." + a.DataSetId
	for _, option := range []containerd.RestoreOpts{containerd.WithRestoreImage, containerd.WithRestoreRW, containerd.WithRestoreRuntime} {
		// These native restore options consume the index; no temporary named image is needed.
		if err = option(ctx, key, c.client, nil, &index)(ctx, c.client, &info); err != nil {
			return err
		}
	}
	for i, volume := range manifest.Volumes {
		path := c.volumeDir(env, a, volume.Id)
		if err = prepareDirectoryVolume(ctx, path, volume.SizeGiB); err != nil {
			return err
		}
		file, err := os.Open(filepath.Join(input, fmt.Sprintf("volume-%d.tar", i)))
		if err != nil {
			return err
		}
		_, applyErr := filesystem.Apply(ctx, path, file)
		if err = errors.Join(applyErr, file.Close()); err != nil {
			return err
		}
	}
	return writeRecoveryJSON(filepath.Join(staging, "container.json"), info)
}

func (v *VirtualMachines) prepareRecovery(ctx context.Context, env string, a api.AssetExecution, input, staging, templateDir string, manifest recoveryManifest) error {
	raw, err := os.ReadFile(filepath.Join(input, "domain.xml"))
	if err != nil {
		return err
	}
	restoreMemory := manifest.Memory && a.InstanceId == manifest.Execution.InstanceId
	if restoreMemory {
		text, err := v.conn.DomainSaveImageGetXMLDesc(filepath.Join(input, "memory.save"), 0)
		if err != nil {
			return err
		}
		raw = []byte(text)
	}
	var domain libvirtxml.Domain
	if err = domain.Unmarshal(string(raw)); err != nil {
		return err
	}
	// Host security labels belong to the target libvirt, not the captured guest.
	domain.SecLabel = nil
	domain.Name, domain.UUID = "netlab-"+a.InstanceId, a.InstanceId
	directory := assetDirectory(v.data, env, a)
	disks := executionDiskMap(v.data, env, a)
	sourceDisks := executionDiskMap(v.data, env, manifest.Execution)
	for _, disk := range manifest.Disks {
		if disk.BackingTemplateDisk != nil {
			source := systemDiskPath(templateDir, *disk.BackingTemplateDisk)
			if err = command(ctx, "qemu-img", "rebase", "-u", "-f", "qcow2", "-F", "qcow2", "-b", source, filepath.Join(input, disk.File)); err != nil {
				return err
			}
		}
		target, exists := disks[disk.Serial]
		if !exists {
			return fmt.Errorf("captured disk %s is not present in the recovery execution", disk.Serial)
		}
		if target.rbd == nil && filepath.Dir(target.file) == directory {
			target.file = filepath.Join(staging, filepath.Base(target.file))
		}
		if disk.RBDImage != "" {
			source := sourceDisks[disk.Serial]
			source.root, source.image, source.snapshot = target.root, disk.RBDImage, disk.RBDSnapshot
			if sharedDisk(source, target) {
				continue
			}
			err = target.restoreSnapshot(ctx, source)
		} else {
			err = target.restoreFile(ctx, filepath.Join(input, disk.File))
		}
		if err != nil {
			return err
		}
	}
	for i := range domain.Devices.Disks {
		disk := &domain.Devices.Disks[i]
		if disk.Device == "disk" {
			location := disks[disk.Serial]
			disk.Source, disk.Auth, err = location.source()
			if err != nil {
				return err
			}
			disk.Driver.Type = location.format()
		} else if disk.Source != nil && disk.Source.File != nil {
			disk.Source.File.File = filepath.Join(directory, filepath.Base(disk.Source.File.File))
		}
	}
	files := []string{"nvram.fd", "tpm.tar", "initialization.iso"}
	if restoreMemory {
		files = append(files, "memory.save")
	}
	for _, name := range files {
		if err = os.Rename(filepath.Join(input, name), filepath.Join(staging, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if a.Template.Media != nil {
		for i := range *a.Template.Media {
			if err = os.Symlink(templateMediaPath(templateDir, i), templateMediaPath(staging, i)); err != nil {
				return err
			}
		}
	}
	if domain.OS.NVRam != nil {
		domain.OS.NVRam.NVRam = filepath.Join(directory, "nvram.fd")
	}
	for i := range domain.Devices.Interfaces {
		iface := &domain.Devices.Interfaces[i]
		nic := a.Interfaces[i]
		iface.MAC.Address = nic.Mac
		iface.Source.Bridge.Bridge = v.bridge
		iface.VirtualPort.Params.OpenVSwitch.InterfaceID = nic.PortName
		iface.MTU = &libvirtxml.DomainInterfaceMTU{Size: uint(nic.Mtu)}
		iface.Target = nil
	}
	for i := range domain.Devices.Channels {
		channel := &domain.Devices.Channels[i]
		if channel.Source != nil && channel.Source.UNIX != nil {
			channel.Source.UNIX.Path = ""
		}
	}
	if domain.Devices.Hostdevs, err = domainHostDevices(a.PciDevices); err != nil {
		return err
	}
	execution, err := json.Marshal(a)
	if err != nil {
		return err
	}
	metadata, err := xml.Marshal(Ownership{Environment: env, Asset: a.Asset.Id, Instance: a.InstanceId, Execution: string(execution)})
	if err != nil {
		return err
	}
	domain.Metadata = &libvirtxml.DomainMetadata{XML: string(metadata)}
	if !restoreMemory && (a.InstanceId != manifest.Execution.InstanceId || a.DataSetId != manifest.Execution.DataSetId) {
		domain.GenID = &libvirtxml.DomainGenID{Value: uuid.NewString()}
	}
	config, err := domain.Marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(staging, "domain.xml"), []byte(config), 0600)
}

// Native metadata and TPM are committed before either runtime is replaced.
func saveRecoveryNative(directory string, save func(string) error) error {
	if _, err := os.Stat(directory); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging := directory + ".pending"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0711); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := save(staging); err != nil {
		return err
	}
	return os.Rename(staging, directory)
}

func (c *Containers) applyRecovery(ctx context.Context, env string, a api.AssetExecution) (string, error) {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	ctx = leases.WithLease(ctx, recoveryLeaseID(a))
	directory := assetDirectory(c.data, env, a)
	container, err := c.client.LoadContainer(ctx, a.InstanceId)
	if err == nil {
		labels, err := container.Labels(ctx)
		if err != nil {
			return "unknown", err
		}
		if !managedContainer(labels, c.node) || labels[environmentLabel] != env || labels[assetLabel] != a.Asset.Id {
			return "unknown", errors.New("container ownership does not match recovery")
		}
		observed, err := c.observedExecution(ctx, container)
		if err != nil {
			return "unknown", err
		}
		if observed.DataSetId == a.DataSetId {
			return containerState(ctx, container)
		}
		state, err := containerState(ctx, container)
		if err != nil || !matchesStopped(state) {
			return state, errors.Join(err, errors.New("container must be stopped before recovery"))
		}
		if err = saveRecoveryNative(filepath.Join(directory, "before"), func(path string) error {
			info, err := container.Info(ctx)
			if err != nil {
				return err
			}
			if err = c.client.LeasesService().AddResource(ctx, leases.Lease{ID: recoveryLeaseID(a)}, leases.Resource{Type: "snapshots/" + info.Snapshotter, ID: info.SnapshotKey}); err != nil {
				return err
			}
			return writeRecoveryJSON(filepath.Join(path, "container.json"), info)
		}); err != nil {
			return state, err
		}
		if err = container.Delete(ctx); err != nil {
			return state, err
		}
	} else if !errdefs.IsNotFound(err) {
		return "unknown", err
	}
	info, err := readRecoveryContainer(filepath.Join(directory, "container.json"))
	if err != nil {
		return "absent", err
	}
	image, err := c.client.GetImage(ctx, info.Image)
	if err != nil {
		return "absent", err
	}
	options, _, err := c.containerConfiguration(ctx, env, a, image)
	if err != nil {
		return "absent", err
	}
	_, err = c.client.NewContainer(ctx, a.InstanceId, append([]containerd.NewContainerOpts{func(_ context.Context, _ *containerd.Client, target *containers.Container) error {
		*target = info
		return nil
	}}, options...)...)
	return "stopped", err
}

func matchesStopped(state string) bool { return state == "stopped" || state == "prepared" }

func (v *VirtualMachines) applyRecovery(ctx context.Context, env string, a api.AssetExecution) (string, error) {
	directory := assetDirectory(v.data, env, a)
	domain, err := v.conn.LookupDomainByUUIDString(a.InstanceId)
	if err == nil {
		defer domain.Free()
		if _, err = v.owned(domain, env, a.Asset.Id); err != nil {
			return "unknown", err
		}
		observed, err := v.observedExecution(ctx, domain)
		if err != nil {
			return "unknown", err
		}
		if observed.DataSetId == a.DataSetId {
			return v.restoreRecoveryMemory(domain, directory)
		}
		state, err := vmState(domain)
		if err != nil || state != "stopped" {
			return state, errors.Join(err, errors.New("VM must be stopped before recovery"))
		}
		if err = saveRecoveryNative(filepath.Join(directory, "before"), func(path string) error {
			config, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
			if err != nil {
				return err
			}
			if err = os.WriteFile(filepath.Join(path, "domain.xml"), []byte(config), 0600); err != nil {
				return err
			}
			if observed.Template.Hardware.Tpm != nil && *observed.Template.Hardware.Tpm {
				return archiveTPM(tpmDirectory(a.InstanceId), filepath.Join(path, "tpm.tar"))
			}
			return nil
		}); err != nil {
			return state, err
		}
	} else if !noDomain(err) {
		return "unknown", err
	}
	if err = os.RemoveAll(tpmDirectory(a.InstanceId)); err != nil {
		return "stopped", err
	}
	if a.Template.Hardware.Tpm != nil && *a.Template.Hardware.Tpm {
		if err = restoreTPM(filepath.Join(directory, "tpm.tar"), tpmDirectory(a.InstanceId)); err != nil {
			return "stopped", err
		}
	}
	raw, err := os.ReadFile(filepath.Join(directory, "domain.xml"))
	if err != nil {
		return "stopped", err
	}
	domain, err = v.conn.DomainDefineXML(string(raw))
	if err != nil {
		return "stopped", err
	}
	defer domain.Free()
	return v.restoreRecoveryMemory(domain, directory)
}

func (v *VirtualMachines) restoreRecoveryMemory(domain *libvirt.Domain, directory string) (string, error) {
	state, err := vmState(domain)
	if err != nil {
		return state, err
	}
	path := filepath.Join(directory, "memory.save")
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	text, err := os.ReadFile(filepath.Join(directory, "domain.xml"))
	if err != nil {
		return state, err
	}
	if state == "stopped" {
		if err = v.conn.DomainRestoreFlags(path, string(text), libvirt.DOMAIN_SAVE_PAUSED); err != nil {
			return state, err
		}
	}
	var configured, active libvirtxml.Domain
	if err = configured.Unmarshal(string(text)); err != nil {
		return state, err
	}
	actual, err := domain.GetXMLDesc(0)
	if err != nil {
		return state, err
	}
	if err = active.Unmarshal(actual); err != nil {
		return state, err
	}
	configured.GenID = active.GenID
	persistent, err := configured.Marshal()
	if err != nil {
		return state, err
	}
	defined, err := v.conn.DomainDefineXML(persistent)
	if err != nil {
		return state, err
	}
	if err = defined.Free(); err != nil {
		return state, err
	}
	return vmState(domain)
}

func (e *Engine) removeRecoveryData(ctx context.Context, env string, a api.AssetExecution, preserveShared bool) error {
	for _, source := range a.VolumeSources {
		if source.Kind == api.Container {
			if err := e.Volume(ctx, "delete", source); err != nil {
				return err
			}
		}
	}
	if a.Template.Kind == api.Container {
		ctx = namespaces.WithNamespace(ctx, "netlab")
		err := e.container.client.SnapshotService("overlayfs").Remove(ctx, a.InstanceId+"."+a.DataSetId)
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		if err = e.container.client.LeasesService().Delete(ctx, leases.Lease{ID: recoveryLeaseID(a)}); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if a.Template.Kind == api.Vm {
		for _, disk := range executionDisks(e.cfg.DataDir, env, a) {
			if preserveShared && disk.rbd != nil {
				continue
			}
			if err := disk.remove(ctx); err != nil {
				return err
			}
			if disk.rbd != nil {
				pending := disk
				pending.image += ".pending"
				if err := pending.remove(ctx); err != nil {
					return err
				}
			}
		}
	}
	volumes := filepath.Join(storageRoot(e.cfg.DataDir, a), "environments", env, "volumes", a.Asset.Id, a.DataSetId)
	if a.Template.Kind == api.Container {
		if err := removeDirectoryVolumes(volumes); err != nil {
			return err
		}
	}
	directory := assetDirectory(e.cfg.DataDir, env, a)
	for _, path := range []string{directory, directory + ".pending", volumes} {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) rollbackRecovery(ctx context.Context, env string, original, next api.AssetExecution) (string, error) {
	operation := next.DataSetId
	before := filepath.Join(assetDirectory(e.cfg.DataDir, env, next), "before")
	state := "destroyed"
	if original.Template.Kind == api.Container {
		ctx = namespaces.WithNamespace(ctx, "netlab")
		container, err := e.container.client.LoadContainer(ctx, original.InstanceId)
		if err == nil {
			labels, err := container.Labels(ctx)
			if err != nil {
				return "unknown", err
			}
			if !managedContainer(labels, e.container.node) || labels[environmentLabel] != env || labels[assetLabel] != original.Asset.Id {
				return "unknown", errors.New("container ownership does not match recovery")
			}
			actual, err := e.container.observedExecution(ctx, container)
			if err != nil {
				return "unknown", err
			}
			if actual.DataSetId == operation {
				if err = e.container.stop(ctx, container, *actual, true); err != nil {
					return "unknown", err
				}
				if err = container.Delete(ctx); err != nil {
					return "stopped", err
				}
			} else {
				state, err = containerState(ctx, container)
				return state, errors.Join(err, e.removeRecoveryData(ctx, env, next, false))
			}
		} else if !errdefs.IsNotFound(err) {
			return "unknown", err
		}
		info, err := readRecoveryContainer(filepath.Join(before, "container.json"))
		if err == nil {
			if _, err = e.container.client.NewContainer(ctx, original.InstanceId, func(_ context.Context, _ *containerd.Client, target *containers.Container) error {
				*target = info
				return nil
			}); err != nil {
				return "absent", err
			}
			state = "stopped"
		} else if !errors.Is(err, os.ErrNotExist) {
			return "absent", err
		}
	} else {
		domain, err := e.vm.conn.LookupDomainByUUIDString(original.InstanceId)
		if err == nil {
			if _, err = e.vm.owned(domain, env, original.Asset.Id); err != nil {
				domain.Free()
				return "unknown", err
			}
			actual, readErr := e.vm.observedExecution(ctx, domain)
			if readErr != nil {
				domain.Free()
				return "unknown", readErr
			}
			_, backupErr := os.Stat(filepath.Join(before, "domain.xml"))
			if actual.DataSetId != operation && errors.Is(backupErr, os.ErrNotExist) {
				state, err = vmState(domain)
				domain.Free()
				return state, errors.Join(err, e.removeRecoveryData(ctx, env, next, false))
			}
			state, err = vmState(domain)
			if err == nil && state != "stopped" {
				err = domain.Destroy()
			}
			if err == nil {
				err = domain.UndefineFlags(libvirt.DOMAIN_UNDEFINE_KEEP_NVRAM | libvirt.DOMAIN_UNDEFINE_KEEP_TPM)
			}
			domain.Free()
			if err != nil {
				return state, err
			}
		} else if !noDomain(err) {
			return "unknown", err
		}
		raw, err := os.ReadFile(filepath.Join(before, "domain.xml"))
		if err == nil {
			if original.Template.Hardware.Tpm != nil && *original.Template.Hardware.Tpm {
				if err = os.RemoveAll(tpmDirectory(original.InstanceId)); err == nil {
					err = restoreTPM(filepath.Join(before, "tpm.tar"), tpmDirectory(original.InstanceId))
				}
				if err != nil {
					return "stopped", err
				}
			}
			domain, err = e.vm.conn.DomainDefineXML(string(raw))
			if err != nil {
				return "absent", err
			}
			domain.Free()
			state = "stopped"
		} else if errors.Is(err, os.ErrNotExist) {
			if err = os.RemoveAll(tpmDirectory(original.InstanceId)); err != nil {
				return "destroyed", err
			}
			state = "destroyed"
		} else {
			return "absent", err
		}
	}
	return state, e.removeRecoveryData(ctx, env, next, false)
}

func (e *Engine) cleanupRecovery(ctx context.Context, env string, original, next api.AssetExecution) error {
	directory := assetDirectory(e.cfg.DataDir, env, next)
	if original.DataSetId != next.DataSetId {
		if original.Template.Kind == api.Container {
			ctx = namespaces.WithNamespace(ctx, "netlab")
			info, err := readRecoveryContainer(filepath.Join(directory, "before", "container.json"))
			if err == nil {
				if err = e.container.client.SnapshotService(info.Snapshotter).Remove(ctx, info.SnapshotKey); err != nil && !errdefs.IsNotFound(err) {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err = e.container.removeFiles(ctx, env, original); err != nil {
				return err
			}
		} else {
			if original.Rbd != nil {
				for i := range *original.Template.Disks {
					if err := systemDisk(assetDirectory(e.cfg.DataDir, env, original), original, i).remove(ctx); err != nil {
						return err
					}
				}
			}
			if err := os.RemoveAll(assetDirectory(e.cfg.DataDir, env, original)); err != nil {
				return err
			}
			references, err := e.vm.volumeReferences(env, original.Asset.Id)
			if err != nil {
				return err
			}
			if err = e.vm.removeVolumes(ctx, env, original, references, false); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"before", "before.pending", "manifest.json", "container.json", "domain.xml", "tpm.tar"} {
		if err := os.RemoveAll(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	if original.Template.Kind == api.Container {
		ctx = namespaces.WithNamespace(ctx, "netlab")
		if err := e.container.client.LeasesService().Delete(ctx, leases.Lease{ID: recoveryLeaseID(next)}); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	return nil
}
