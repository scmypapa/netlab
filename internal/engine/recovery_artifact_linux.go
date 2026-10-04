//go:build linux

package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"netlab.local/core/api"
)

func (e *Engine) copyRecovery(ctx context.Context, env, point string, a api.AssetExecution, target, destinationPool string) error {
	directory := recoveryDirectory(e.cfg.DataDir, point, a)
	manifest, err := readRecoveryManifest(directory, env, point, a)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "manifest.json" {
			continue
		}
		if err = copyArtifact(ctx, filepath.Join(directory, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	if a.Rbd != nil && *a.StoragePoolId != destinationPool {
		for i := range manifest.Disks {
			disk := &manifest.Disks[i]
			if disk.RBDImage == "" {
				continue
			}
			source := vmDisk{rbd: a.Rbd, root: *a.StoragePath, image: disk.RBDImage, snapshot: disk.RBDSnapshot}
			if err = command(ctx, "qemu-img", "convert", "-f", "raw", "-O", "qcow2", source.address(), filepath.Join(target, disk.File)); err != nil {
				return err
			}
			disk.RBDImage, disk.RBDSnapshot = "", ""
		}
	}
	return writeRecoveryJSON(filepath.Join(target, "manifest.json"), manifest)
}

type recoveryArtifact struct {
	io.ReadCloser
	directory string
}

func (r *recoveryArtifact) Close() error {
	return errors.Join(r.ReadCloser.Close(), os.RemoveAll(r.directory))
}

func (e *Engine) OpenRecoveryArtifact(ctx context.Context, env, point string, a api.AssetExecution, destinationPool string) (io.ReadCloser, int64, error) {
	directory := recoveryDirectory(e.cfg.DataDir, point, a)
	if _, err := readRecoveryManifest(directory, env, point, a); err != nil {
		return nil, 0, err
	}
	if a.Rbd == nil || *a.StoragePoolId == destinationPool {
		return openDirectoryArtifact(directory)
	}
	staging, err := os.MkdirTemp(filepath.Dir(directory), "export-")
	if err != nil {
		return nil, 0, err
	}
	if err = e.copyRecovery(ctx, env, point, a, staging, destinationPool); err != nil {
		return nil, 0, errors.Join(err, os.RemoveAll(staging))
	}
	reader, size, err := openDirectoryArtifact(staging)
	if err != nil {
		return nil, 0, errors.Join(err, os.RemoveAll(staging))
	}
	return &recoveryArtifact{ReadCloser: reader, directory: staging}, size, nil
}
