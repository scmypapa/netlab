//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	filesystem "github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images/archive"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/platforms"
	"libvirt.org/go/libvirt"
	"libvirt.org/go/libvirtxml"
	"netlab.local/core/api"
)

type recoveryDisk struct {
	Serial              string `json:"serial"`
	File                string `json:"file"`
	BackingTemplateDisk *int   `json:"backingTemplateDisk,omitempty"`
	RBDImage            string `json:"rbdImage,omitempty"`
	RBDSnapshot         string `json:"rbdSnapshot,omitempty"`
}

type recoveryManifest struct {
	EnvironmentID string                  `json:"environmentId"`
	PointID       string                  `json:"pointId"`
	Execution     api.AssetExecution      `json:"execution"`
	Disks         []recoveryDisk          `json:"disks,omitempty"`
	Volumes       []api.Volume            `json:"volumes,omitempty"`
	Bytes         int64                   `json:"bytes"`
	Memory        bool                    `json:"memory,omitempty"`
	Consistency   api.RecoveryConsistency `json:"consistency"`
}

func (m recoveryManifest) captured() api.RecoveryCapture {
	return api.RecoveryCapture{SizeBytes: m.Bytes, Memory: m.Memory, Consistency: m.Consistency}
}

func recoveryDirectory(data, point string, a api.AssetExecution) string {
	return filepath.Join(storageRoot(data, a), "recovery-points", point, a.Asset.Id)
}

func (e *Engine) captureRecovery(ctx context.Context, env, point string, a api.AssetExecution, includeMemory, quiesce bool) (api.RecoveryCapture, error) {
	directory := recoveryDirectory(e.cfg.DataDir, point, a)
	manifestPath := filepath.Join(directory, "manifest.json")
	if raw, err := os.ReadFile(manifestPath); err == nil {
		manifest := recoveryManifest{Consistency: api.Crash}
		if err = json.Unmarshal(raw, &manifest); err != nil {
			return api.RecoveryCapture{}, err
		}
		if manifest.EnvironmentID != env || manifest.PointID != point || manifest.Execution.InstanceId != a.InstanceId {
			return api.RecoveryCapture{}, errors.New("recovery identity does not match capture")
		}
		return manifest.captured(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return api.RecoveryCapture{}, err
	}
	// This stable staging name is exclusively owned by the persisted capture task.
	staging := directory + ".pending"
	if a.Template.Kind == api.Vm {
		if e.vm == nil {
			return api.RecoveryCapture{}, errors.New("virtual machine runtime not configured")
		}
		if err := e.vm.finishRecoveryFS(env, point, a, true); err != nil {
			return api.RecoveryCapture{}, err
		}
	}
	if err := os.RemoveAll(staging); err != nil {
		return api.RecoveryCapture{}, err
	}
	if err := os.MkdirAll(staging, 0711); err != nil {
		return api.RecoveryCapture{}, err
	}
	defer os.RemoveAll(staging)
	manifest := recoveryManifest{EnvironmentID: env, PointID: point, Execution: a, Consistency: api.Crash}
	var err error
	switch a.Template.Kind {
	case api.Vm:
		err = e.vm.captureRecovery(ctx, env, a, staging, &manifest, includeMemory, quiesce)
	case api.Container:
		if e.container == nil {
			return api.RecoveryCapture{}, errors.New("container runtime not configured")
		}
		err = e.container.captureRecovery(ctx, env, point, a, staging, &manifest)
	default:
		return api.RecoveryCapture{}, fmt.Errorf("invalid compute kind %s", a.Template.Kind)
	}
	if err != nil {
		return api.RecoveryCapture{}, err
	}
	for _, disk := range manifest.Disks {
		if disk.RBDImage != "" {
			source := executionDiskMap(e.cfg.DataDir, env, a)[disk.Serial]
			source.image, source.snapshot = disk.RBDImage, disk.RBDSnapshot
			bytes, err := source.usedBytes(ctx)
			if err != nil {
				return api.RecoveryCapture{}, err
			}
			manifest.Bytes += bytes
		}
	}
	if err = filepath.WalkDir(staging, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			manifest.Bytes += info.Sys().(*syscall.Stat_t).Blocks * 512
		}
		return err
	}); err != nil {
		return api.RecoveryCapture{}, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return api.RecoveryCapture{}, err
	}
	if err = os.WriteFile(filepath.Join(staging, "manifest.json"), raw, 0600); err != nil {
		return api.RecoveryCapture{}, err
	}
	if err = os.Rename(staging, directory); err != nil {
		return api.RecoveryCapture{}, err
	}
	return manifest.captured(), nil
}

func (v *VirtualMachines) captureRecovery(ctx context.Context, env string, a api.AssetExecution, directory string, manifest *recoveryManifest, includeMemory, quiesce bool) error {
	domain, err := v.conn.LookupDomainByUUIDString(a.InstanceId)
	if err != nil {
		return err
	}
	defer domain.Free()
	if _, err = v.owned(domain, env, a.Asset.Id); err != nil {
		return err
	}
	state, err := vmState(domain)
	if err != nil {
		return err
	}
	if state != "stopped" && state != "suspended" && state != "running" {
		return fmt.Errorf("VM %s must be quiesced at capture boundary", a.Asset.Name)
	}
	running := state == "running" || quiesce
	frozen := false
	if quiesce && state != "stopped" {
		frozen, err = v.freezeRecoveryFS(domain, manifest.PointID, a, state == "suspended")
		if err != nil {
			return err
		}
	}
	if state == "running" && !frozen {
		if err = domain.Suspend(); err != nil {
			return err
		}
	}
	if state == "running" {
		state = "suspended"
	}
	text, err := domain.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
	if err != nil {
		return err
	}
	config := libvirtxml.Domain{}
	if err = config.Unmarshal(text); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(directory, "domain.xml"), []byte(text), 0600); err != nil {
		return err
	}
	if includeMemory && state == "suspended" {
		if err = captureRecoveryMemory(domain, filepath.Join(directory, "memory.save"), config.Devices.Disks); err != nil {
			return err
		}
		manifest.Memory = true
	}
	backupDisks := []libvirtxml.DomainBackupPushDisk{}
	storage, err := StorageInfo(directory)
	if err != nil {
		return err
	}
	for _, disk := range config.Devices.Disks {
		if disk.Device != "disk" {
			continue
		}
		location, err := diskFromDomain(disk, a)
		if err != nil {
			return err
		}
		file := fmt.Sprintf("disk-%d.qcow2", len(manifest.Disks))
		captured := recoveryDisk{Serial: disk.Serial, File: file}
		if location.rbd != nil {
			if err = location.capture(ctx, manifest.PointID); err != nil {
				return err
			}
			captured.RBDImage, captured.RBDSnapshot = location.image, manifest.PointID
		} else if storage.NativeSnapshots && (state == "stopped" || frozen) {
			if err = cloneFile(location.file, filepath.Join(directory, file)); err != nil {
				return err
			}
			image, err := inspectImage(ctx, filepath.Join(directory, file))
			if err != nil {
				return err
			}
			if image.BackingFilename != "" {
				for i, definition := range *a.Template.Disks {
					if disk.Serial == "image-"+definition.Id {
						captured.BackingTemplateDisk = ptr(i)
						break
					}
				}
				if captured.BackingTemplateDisk == nil {
					return fmt.Errorf("disk %s has an unmanaged backing image", disk.Serial)
				}
			}
		} else if state == "suspended" {
			backupDisks = append(backupDisks, libvirtxml.DomainBackupPushDisk{Name: disk.Target.Dev, Backup: "yes",
				Driver: &libvirtxml.DomainBackupDiskDriver{Type: "qcow2"},
				Target: &libvirtxml.DomainDiskSource{File: &libvirtxml.DomainDiskSourceFile{File: filepath.Join(directory, file)}}})
		} else {
			image, err := inspectImage(ctx, location.file)
			if err != nil {
				return err
			}
			if err = command(ctx, "qemu-img", "convert", "-f", image.Format, "-O", "qcow2", location.file, filepath.Join(directory, file)); err != nil {
				return fmt.Errorf("disk %s: %w", disk.Serial, err)
			}
		}
		manifest.Disks = append(manifest.Disks, captured)
	}
	if config.OS.NVRam != nil {
		if err = copyArtifact(ctx, config.OS.NVRam.NVRam, filepath.Join(directory, "nvram.fd")); err != nil {
			return err
		}
	}
	if len(config.Devices.TPMs) > 0 {
		if err = archiveTPM(tpmDirectory(a.InstanceId), filepath.Join(directory, "tpm.tar")); err != nil {
			return err
		}
	}
	initialization := filepath.Join(assetDirectory(v.data, env, a), "initialization.iso")
	if _, err = os.Stat(initialization); err == nil {
		if err = copyArtifact(ctx, initialization, filepath.Join(directory, "initialization.iso")); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	resume := func() error {
		if frozen {
			raw, err := os.ReadFile(recoveryFreezePath(v.data, manifest.PointID, a))
			if err != nil {
				return err
			}
			consistent, err := v.thawRecoveryFS(domain, manifest.PointID, a, !running)
			if err == nil && consistent {
				manifest.Consistency = api.RecoveryConsistency(raw)
			}
			return err
		}
		if running {
			return domain.Resume()
		}
		return nil
	}
	if len(backupDisks) > 0 {
		return backupRecoveryDisks(ctx, domain, backupDisks, resume)
	}
	return resume()
}

func (c *Containers) captureRecovery(ctx context.Context, env, point string, a api.AssetExecution, directory string, manifest *recoveryManifest) (err error) {
	ctx = namespaces.WithNamespace(ctx, "netlab")
	container, err := c.client.LoadContainer(ctx, a.InstanceId)
	if err != nil {
		return err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return err
	}
	if !managedContainer(labels, c.node) || labels[environmentLabel] != env || labels[assetLabel] != a.Asset.Id {
		return errors.New("container ownership does not match capture")
	}
	state, err := containerState(ctx, container)
	if err != nil {
		return err
	}
	if state != "stopped" && state != "suspended" {
		return fmt.Errorf("container %s must be quiesced at capture boundary", a.Asset.Name)
	}
	ref := "netlab/recovery/" + point + "/" + a.Asset.Id
	// A previous interrupted export may leave its temporary native image registered.
	if err = c.client.ImageService().Delete(ctx, ref); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	checkpoint, err := container.Checkpoint(ctx, ref, containerd.WithCheckpointImage, containerd.WithCheckpointRW)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, c.client.ImageService().Delete(context.WithoutCancel(ctx), ref)) }()
	file, err := os.OpenFile(filepath.Join(directory, "container.tar"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	exportErr := c.client.Export(ctx, file, archive.WithManifest(checkpoint.Target(), ref), archive.WithPlatform(platforms.Default()), archive.WithSkipMissing(c.client.ContentStore()), archive.WithSkipDockerManifest())
	if err = errors.Join(exportErr, file.Close()); err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(labels["netlab.volumes"]), &manifest.Volumes); err != nil {
		return err
	}
	for i, volume := range manifest.Volumes {
		file, err := os.OpenFile(filepath.Join(directory, fmt.Sprintf("volume-%d.tar", i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		archiveErr := filesystem.WriteDiff(ctx, file, "", c.volumeDir(env, a, volume.Id))
		if err = errors.Join(archiveErr, file.Close()); err != nil {
			return fmt.Errorf("volume %s: %w", volume.Id, err)
		}
	}
	return nil
}

func captureRecoveryMemory(domain *libvirt.Domain, path string, disks []libvirtxml.DomainDisk) error {
	plan := libvirtxml.DomainSnapshot{Memory: &libvirtxml.DomainSnapshotMemory{Snapshot: "external", File: path}, Disks: &libvirtxml.DomainSnapshotDisks{}}
	for _, disk := range disks {
		plan.Disks.Disks = append(plan.Disks.Disks, libvirtxml.DomainSnapshotDisk{Name: disk.Target.Dev, Snapshot: "no"})
	}
	text, err := plan.Marshal()
	if err != nil {
		return err
	}
	snapshot, err := domain.CreateSnapshotXML(text, libvirt.DOMAIN_SNAPSHOT_CREATE_NO_METADATA)
	if err != nil {
		return err
	}
	return snapshot.Free()
}

func backupRecoveryDisks(ctx context.Context, domain *libvirt.Domain, disks []libvirtxml.DomainBackupPushDisk, atBoundary func() error) error {
	active, err := domain.GetJobStats(0)
	if err != nil {
		return err
	}
	if active.Type != libvirt.DOMAIN_JOB_NONE {
		text, err := domain.BackupGetXMLDesc(0)
		if err != nil {
			current, inspectErr := domain.GetJobStats(0)
			if inspectErr != nil || current.Type != libvirt.DOMAIN_JOB_NONE {
				return errors.Join(err, inspectErr)
			}
		} else {
			var running libvirtxml.DomainBackup
			if err = running.Unmarshal(text); err != nil {
				return err
			}
			if running.Push == nil || running.Push.Disks == nil {
				return errors.New("VM has another native backup in progress")
			}
			expected := map[string]string{}
			for _, disk := range disks {
				expected[disk.Name] = disk.Target.File.File
			}
			for _, disk := range running.Push.Disks.Disks {
				if disk.Backup == "no" {
					continue
				}
				if disk.Target == nil || disk.Target.File == nil || expected[disk.Name] != disk.Target.File.File {
					return errors.New("VM has another native backup in progress")
				}
			}
			// A node restart can leave the persisted capture block job alive.
			if err = domain.AbortJob(); err != nil {
				current, inspectErr := domain.GetJobStats(0)
				if inspectErr != nil || current.Type != libvirt.DOMAIN_JOB_NONE {
					return errors.Join(err, inspectErr)
				}
			}
		}
	}
	plan := libvirtxml.DomainBackup{Push: &libvirtxml.DomainBackupPush{Disks: &libvirtxml.DomainBackupPushDisks{Disks: disks}}}
	text, err := plan.Marshal()
	if err != nil {
		return err
	}
	if err = domain.BackupBegin(text, "", 0); err != nil {
		return err
	}
	if err = atBoundary(); err != nil {
		return errors.Join(err, domain.AbortJob())
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		stats, err := domain.GetJobStats(0)
		if err != nil {
			return errors.Join(err, domain.AbortJob())
		}
		if stats.Type == libvirt.DOMAIN_JOB_NONE {
			stats, err = domain.GetJobStats(libvirt.DOMAIN_JOB_STATS_COMPLETED)
			if err != nil {
				return err
			}
		}
		switch stats.Type {
		case libvirt.DOMAIN_JOB_COMPLETED:
			return nil
		case libvirt.DOMAIN_JOB_NONE, libvirt.DOMAIN_JOB_FAILED, libvirt.DOMAIN_JOB_CANCELLED:
			return fmt.Errorf("native disk backup did not complete: %s", stats.ErrorMessage)
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), domain.AbortJob())
		case <-ticker.C:
		}
	}
}

func (e *Engine) deleteRecovery(ctx context.Context, env, point string, a api.AssetExecution) error {
	if a.Template.Kind == api.Vm && e.vm != nil {
		if err := e.vm.finishRecoveryFS(env, point, a, false); err != nil {
			return err
		}
	}
	if a.Template.Kind == api.Vm {
		if err := e.vm.deleteDiskSnapshots(ctx, env, point, a); err != nil {
			return err
		}
	}
	directory := recoveryDirectory(e.cfg.DataDir, point, a)
	return errors.Join(os.RemoveAll(directory), os.RemoveAll(directory+".pending"))
}
