package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestArchivePermissionsIgnoreServiceUmask(t *testing.T) {
	for name, mask := range map[string]int{"controller": 0027, "node": 0077} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			var data bytes.Buffer
			gz := gzip.NewWriter(&data)
			tarball := tar.NewWriter(gz)
			for _, header := range []*tar.Header{
				{Name: "guacamole/", Typeflag: tar.TypeDir, Mode: 0755},
				{Name: "guacamole/sbin/guacd", Typeflag: tar.TypeReg, Mode: 0755},
				{Name: "web/assets/index.html", Typeflag: tar.TypeReg, Mode: 0644},
			} {
				if err := tarball.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
			}
			if err := errors.Join(tarball.Close(), gz.Close()); err != nil {
				t.Fatal(err)
			}
			archive := filepath.Join(directory, "package.tar.gz")
			if err := os.WriteFile(archive, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			previous := syscall.Umask(mask)
			defer syscall.Umask(previous)
			destination := filepath.Join(directory, "release")
			if err := extract(archive, destination); err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]os.FileMode{
				".": 0755, "guacamole": 0755, "guacamole/sbin": 0755,
				"guacamole/sbin/guacd": 0755, "web": 0755,
				"web/assets": 0755, "web/assets/index.html": 0644,
			} {
				info, err := os.Stat(filepath.Join(destination, path))
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != want {
					t.Errorf("%s mode = %04o, want %04o", path, got, want)
				}
			}
		})
	}
}
