//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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

func (e *Engine) OpenRecoveryArtifact(env, point string, a api.AssetExecution) (io.ReadCloser, int64, error) {
	directory := recoveryDirectory(e.cfg.DataDir, point, a)
	if _, err := readRecoveryManifest(directory, env, point, a); err != nil {
		return nil, 0, err
	}
	return openDirectoryArtifact(directory)
}

func (e *Engine) prepareRecovery(ctx context.Context, plan api.NodePlan, a api.AssetExecution) (err error) {
	source, ok := (*plan.RecoverySources)[a.Asset.Id]
	if !ok || source.Execution.Asset.Id != a.Asset.Id {
		return errors.New("recovery source does not match target")
	}
	directory := assetDirectory(e.cfg.DataDir, plan.EnvironmentId, a)
	if _, err = readRecoveryManifest(directory, source.EnvironmentId, *plan.RecoveryPointId, source.Execution); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Preparation owns only this operation's new data, never the current instance data.
	if err = e.removeRecoveryData(ctx, plan.EnvironmentId, a); err != nil {
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
			err = errors.Join(err, e.removeRecoveryData(context.WithoutCancel(ctx), plan.EnvironmentId, a))
		}
	}()
	input := filepath.Join(staging, "input")
	if source.NodeId == e.cfg.ID && source.Backup == nil {
		sourceDir := recoveryDirectory(e.cfg.DataDir, *plan.RecoveryPointId, source.Execution)
		entries, readErr := os.ReadDir(sourceDir)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			if err = copyArtifact(ctx, filepath.Join(sourceDir, entry.Name()), filepath.Join(input, entry.Name())); err != nil {
				return err
			}
		}
	} else {
		reader, _, sourceErr := e.openRecoverySource(ctx, *plan.RecoveryPointId, source)
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
	switch a.Template.Kind {
	case api.Container:
		err = e.container.prepareRecovery(ctx, plan.EnvironmentId, a, input, staging, manifest)
	case api.Vm:
		err = e.vm.prepareRecovery(ctx, plan.EnvironmentId, a, input, staging, manifest)
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
		if err = os.MkdirAll(path, 0755); err != nil {
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

func (v *VirtualMachines) prepareRecovery(ctx context.Context, env string, a api.AssetExecution, input, staging string, manifest recoveryManifest) error {
	raw, err := os.ReadFile(filepath.Join(input, "domain.xml"))
	if err != nil {
		return err
	}
	var domain libvirtxml.Domain
	if err = domain.Unmarshal(string(raw)); err != nil {
		return err
	}
	domain.Name, domain.UUID = "netlab-"+a.InstanceId, a.InstanceId
	directory := assetDirectory(v.data, env, a)
	disks := map[string]string{}
	for i, disk := range *a.Template.Disks {
		disks["image-"+disk.Id] = systemDiskPath(directory, i)
	}
	if a.Asset.Volumes != nil {
		for _, volume := range *a.Asset.Volumes {
			disks["volume-"+volume.Id] = v.volumePath(env, a, volume.Id)
		}
	}
	for _, disk := range manifest.Disks {
		target := disks[disk.Serial]
		if target == "" {
			return fmt.Errorf("captured disk %s is not present in the recovery execution", disk.Serial)
		}
		if filepath.Dir(target) == directory {
			target = filepath.Join(staging, filepath.Base(target))
		}
		if err = os.MkdirAll(filepath.Dir(target), 0711); err != nil {
			return err
		}
		if err = os.Rename(filepath.Join(input, disk.File), target); err != nil {
			return err
		}
	}
	for i := range domain.Devices.Disks {
		disk := &domain.Devices.Disks[i]
		if disk.Device == "disk" {
			disk.Source = &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: disks[disk.Serial]}}
		} else if disk.Source != nil && disk.Source.File != nil {
			disk.Source.File.File = filepath.Join(directory, filepath.Base(disk.Source.File.File))
		}
	}
	for _, name := range []string{"nvram.fd", "tpm.tar", "initialization.iso"} {
		if err = os.Rename(filepath.Join(input, name), filepath.Join(staging, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if a.Template.Media != nil {
		for i := range *a.Template.Media {
			if err = os.Symlink(templateMediaPath(templateDirectory(v.data, a.Template.Id, a.Template.Version), i), templateMediaPath(staging, i)); err != nil {
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
	execution, err := json.Marshal(a)
	if err != nil {
		return err
	}
	metadata, err := xml.Marshal(Ownership{Environment: env, Asset: a.Asset.Id, Instance: a.InstanceId, Execution: string(execution)})
	if err != nil {
		return err
	}
	domain.Metadata = &libvirtxml.DomainMetadata{XML: string(metadata)}
	domain.GenID = &libvirtxml.DomainGenID{Value: uuid.NewString()}
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
		observed, err := v.observedExecution(domain)
		if err != nil {
			return "unknown", err
		}
		if observed.DataSetId == a.DataSetId {
			return vmState(domain)
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
	return vmState(domain)
}

func (e *Engine) removeRecoveryData(ctx context.Context, env string, a api.AssetExecution) error {
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
	volumes := filepath.Join(storageRoot(e.cfg.DataDir, a), "environments", env, "volumes", a.Asset.Id, a.DataSetId)
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

func (e *Engine) rollbackRecovery(ctx context.Context, env, operation string, original api.AssetExecution) (string, error) {
	next := original
	next.DataSetId = operation
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
				return state, errors.Join(err, e.removeRecoveryData(ctx, env, next))
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
			actual, readErr := e.vm.observedExecution(domain)
			if readErr != nil {
				domain.Free()
				return "unknown", readErr
			}
			_, backupErr := os.Stat(filepath.Join(before, "domain.xml"))
			if actual.DataSetId != operation && errors.Is(backupErr, os.ErrNotExist) {
				state, err = vmState(domain)
				domain.Free()
				return state, errors.Join(err, e.removeRecoveryData(ctx, env, next))
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
	return state, e.removeRecoveryData(ctx, env, next)
}

func (e *Engine) cleanupRecovery(ctx context.Context, env, operation string, original api.AssetExecution) error {
	next := original
	next.DataSetId = operation
	directory := assetDirectory(e.cfg.DataDir, env, next)
	if original.DataSetId != operation {
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
			if err := os.RemoveAll(assetDirectory(e.cfg.DataDir, env, original)); err != nil {
				return err
			}
			references, err := e.vm.volumeReferences(env, original.Asset.Id)
			if err != nil {
				return err
			}
			if err = removeVolumeFiles(original.Asset.Volumes, references, func(id string) string { return e.vm.volumePath(env, original, id) }, false); err != nil {
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
