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
	Serial string `json:"serial"`
	File   string `json:"file"`
}

type recoveryManifest struct {
	EnvironmentID string             `json:"environmentId"`
	PointID       string             `json:"pointId"`
	Execution     api.AssetExecution `json:"execution"`
	Disks         []recoveryDisk     `json:"disks,omitempty"`
	Volumes       []api.Volume       `json:"volumes,omitempty"`
	Bytes         int64              `json:"bytes"`
}

func recoveryDirectory(data, point string, a api.AssetExecution) string {
	return filepath.Join(storageRoot(data, a), "recovery-points", point, a.Asset.Id)
}

func (e *Engine) captureRecovery(ctx context.Context, env, point string, a api.AssetExecution) (int64, error) {
	directory := recoveryDirectory(e.cfg.DataDir, point, a)
	manifestPath := filepath.Join(directory, "manifest.json")
	if raw, err := os.ReadFile(manifestPath); err == nil {
		var manifest recoveryManifest
		if err = json.Unmarshal(raw, &manifest); err != nil {
			return 0, err
		}
		if manifest.EnvironmentID != env || manifest.PointID != point || manifest.Execution.InstanceId != a.InstanceId {
			return 0, errors.New("recovery identity does not match capture")
		}
		return manifest.Bytes, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	// This stable staging name is exclusively owned by the persisted capture task.
	staging := directory + ".pending"
	if err := os.RemoveAll(staging); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(staging, 0711); err != nil {
		return 0, err
	}
	defer os.RemoveAll(staging)
	manifest := recoveryManifest{EnvironmentID: env, PointID: point, Execution: a}
	var err error
	switch a.Template.Kind {
	case api.Vm:
		if e.vm == nil {
			return 0, errors.New("virtual machine runtime not configured")
		}
		err = e.vm.captureRecovery(ctx, env, a, staging, &manifest)
	case api.Container:
		if e.container == nil {
			return 0, errors.New("container runtime not configured")
		}
		err = e.container.captureRecovery(ctx, env, point, a, staging, &manifest)
	default:
		return 0, fmt.Errorf("invalid compute kind %s", a.Template.Kind)
	}
	if err != nil {
		return 0, err
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
		return 0, err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return 0, err
	}
	if err = os.WriteFile(filepath.Join(staging, "manifest.json"), raw, 0600); err != nil {
		return 0, err
	}
	if err = os.Rename(staging, directory); err != nil {
		return 0, err
	}
	return manifest.Bytes, nil
}

func (v *VirtualMachines) captureRecovery(ctx context.Context, env string, a api.AssetExecution, directory string, manifest *recoveryManifest) error {
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
	if state != "stopped" {
		return fmt.Errorf("VM %s must be stopped at capture boundary", a.Asset.Name)
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
	for _, disk := range config.Devices.Disks {
		if disk.Device != "disk" {
			continue
		}
		if disk.Source == nil || disk.Source.File == nil {
			return errors.New("recovery requires managed file disks")
		}
		file := fmt.Sprintf("disk-%d.qcow2", len(manifest.Disks))
		image, err := inspectImage(ctx, disk.Source.File.File)
		if err != nil {
			return err
		}
		if err = command(ctx, "qemu-img", "convert", "-f", image.Format, "-O", "qcow2", disk.Source.File.File, filepath.Join(directory, file)); err != nil {
			return fmt.Errorf("disk %s: %w", disk.Serial, err)
		}
		manifest.Disks = append(manifest.Disks, recoveryDisk{Serial: disk.Serial, File: file})
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
		return copyArtifact(ctx, initialization, filepath.Join(directory, "initialization.iso"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
	if state != "stopped" {
		return fmt.Errorf("container %s must be stopped at capture boundary", a.Asset.Name)
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

func (e *Engine) deleteRecovery(point string, a api.AssetExecution) error {
	directory := recoveryDirectory(e.cfg.DataDir, point, a)
	return errors.Join(os.RemoveAll(directory), os.RemoveAll(directory+".pending"))
}
