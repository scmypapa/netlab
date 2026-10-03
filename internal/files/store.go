package files

import (
	"context"
	"errors"
	"io"
	"netlab.local/core/api"
	"os"
	"path"
)

type Store interface {
	List(string) ([]api.FileEntry, error)
	Read(string) (io.ReadCloser, int64, error)
	Write(context.Context, string, io.Reader) error
	Mkdir(string) error
	Rename(string, string) error
	Remove(string) error
	Close() error
}

func mutationPath(name string) (string, error) {
	base := path.Base(name)
	name = path.Clean(name)
	if name == "/" || base == "." || base == ".." {
		return "", errors.New("请选择文件或目录")
	}
	return name, nil
}

func Entry(info os.FileInfo) api.FileEntry {
	kind := "other"
	switch {
	case info.IsDir():
		kind = "directory"
	case info.Mode().IsRegular():
		kind = "file"
	case info.Mode()&os.ModeSymlink != 0:
		kind = "link"
	}
	return api.FileEntry{Name: info.Name(), Kind: api.FileEntryKind(kind), Size: info.Size(), ModifiedAt: info.ModTime(), Mode: info.Mode().String()}
}
