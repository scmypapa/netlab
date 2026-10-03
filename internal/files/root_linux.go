//go:build linux

package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
)

type Root struct {
	root     *os.File
	uid, gid int
}

func OpenRoot(root string, uid, gid int) (*Root, error) {
	file, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	return &Root{root: file, uid: uid, gid: gid}, nil
}

func (r *Root) Close() error { return r.root.Close() }

// IN_ROOT gives absolute links and '..' the same meaning as inside the container.
func (r *Root) open(name string, flags int) (*os.File, error) {
	fd, err := unix.Openat2(int(r.root.Fd()), name, &unix.OpenHow{
		Flags: uint64(flags | unix.O_CLOEXEC), Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (r *Root) List(name string) ([]api.FileEntry, error) {
	dir, err := r.open(name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	result := make([]api.FileEntry, 0, len(names))
	for _, name := range names {
		child, err := unix.Openat(int(dir.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(child), name)
		info, err := file.Stat()
		file.Close()
		if err != nil {
			return nil, err
		}
		result = append(result, Entry(info))
	}
	return result, nil
}

func (r *Root) Read(name string) (io.ReadCloser, int64, error) {
	file, err := r.open(name, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("只支持下载普通文件")
	}
	if err != nil {
		file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func (r *Root) parent(name string) (*os.File, string, error) {
	name, err := mutationPath(name)
	if err != nil {
		return nil, "", err
	}
	parent, err := r.open(path.Dir(name), unix.O_RDONLY|unix.O_DIRECTORY)
	return parent, path.Base(name), err
}

func (r *Root) Write(ctx context.Context, name string, input io.Reader) error {
	parent, base, err := r.parent(name)
	if err != nil {
		return err
	}
	defer parent.Close()
	var existing unix.Stat_t
	mode, uid, gid := uint32(0644), r.uid, r.gid
	err = unix.Fstatat(int(parent.Fd()), base, &existing, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if existing.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("上传目标不是普通文件")
		}
		mode, uid, gid = existing.Mode&0777, int(existing.Uid), int(existing.Gid)
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	temp := ".netlab-upload-" + uuid.NewString()
	fd, err := unix.Openat(int(parent.Fd()), temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), temp)
	defer file.Close()
	defer unix.Unlinkat(int(parent.Fd()), temp, 0)
	if _, err = io.Copy(file, input); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = file.Chown(uid, gid); err != nil {
		return err
	}
	if err = file.Chmod(os.FileMode(mode)); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	return unix.Renameat(int(parent.Fd()), temp, int(parent.Fd()), base)
}

func (r *Root) Mkdir(name string) error {
	parent, base, err := r.parent(name)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err = unix.Mkdirat(int(parent.Fd()), base, 0755); err != nil {
		return err
	}
	return unix.Fchownat(int(parent.Fd()), base, r.uid, r.gid, unix.AT_SYMLINK_NOFOLLOW)
}

func (r *Root) Rename(from, to string) error {
	source, src, err := r.parent(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, dst, err := r.parent(to)
	if err != nil {
		return err
	}
	defer target.Close()
	return unix.Renameat2(int(source.Fd()), src, int(target.Fd()), dst, unix.RENAME_NOREPLACE)
}

func (r *Root) Remove(name string) error {
	parent, base, err := r.parent(name)
	if err != nil {
		return err
	}
	defer parent.Close()
	return removeAt(int(parent.Fd()), base)
}

func removeAt(parent int, name string) error {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return unix.Unlinkat(parent, name, 0)
	}
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), name)
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, child := range names {
		if err = removeAt(fd, child); err != nil {
			return err
		}
	}
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}
