//go:build linux

package engine

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"netlab.local/core/api"
)

func tpmDirectory(instance string) string {
	return filepath.Join("/var/lib/libvirt/swtpm", instance)
}

func archiveTPM(directory, destination string) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	archive := tar.NewWriter(file)
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == directory {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("TPM state %s is not a regular file", entry.Name())
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		if entry.Name() == ".lock" {
			return nil
		}
		owner, err := user.LookupId(strconv.Itoa(header.Uid))
		if err != nil {
			return err
		}
		group, err := user.LookupGroupId(strconv.Itoa(header.Gid))
		if err != nil {
			return err
		}
		header.Uname, header.Gname = owner.Username, group.Name
		header.Name, err = filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		if err = archive.WriteHeader(header); err != nil || info.IsDir() {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(archive, input)
		return errors.Join(err, input.Close())
	})
	return errors.Join(err, archive.Close(), file.Close())
}

func restoreTemplateState(ctx context.Context, source, instanceDir, instance string, t api.Template) error {
	if t.StateFiles == nil {
		return nil
	}
	for _, name := range *t.StateFiles {
		switch name {
		case "nvram.fd":
			path := filepath.Join(instanceDir, name)
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err = copyArtifact(ctx, filepath.Join(source, name), path); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		case "tpm.tar":
			directory := tpmDirectory(instance)
			if _, err := os.Stat(directory); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := restoreTPM(filepath.Join(source, name), directory); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported VM state file %s", name)
		}
	}
	return nil
}

func restoreTPM(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err = os.MkdirAll(filepath.Dir(destination), 0711); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err = os.Chmod(staging, 0711); err != nil {
		return err
	}
	archive := tar.NewReader(input)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		path, err := artifactPath(staging, header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(path, 0700); err != nil {
				return err
			}
			if err = restoreStateOwner(path, header); err != nil {
				return err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return errors.New("TPM archive contains an unsupported entry")
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, archive)
		if err = errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
		if err = restoreStateOwner(path, header); err != nil {
			return err
		}
	}
	return os.Rename(staging, destination)
}

func restoreStateOwner(path string, header *tar.Header) error {
	owner, err := user.Lookup(header.Uname)
	if err != nil {
		return err
	}
	group, err := user.LookupGroup(header.Gname)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(owner.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return err
	}
	if err = os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, fs.FileMode(header.Mode)&0777)
}
