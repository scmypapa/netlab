//go:build linux

package engine

import (
	"bytes"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"
)

func TestTemplateUploadKeepsRelativeFilesAndCleansFailedStreams(t *testing.T) {
	for _, test := range []struct {
		name, main string
		files      []string
		truncate   bool
		valid      bool
	}{
		{"ovf with nested disks", "vm/machine.ovf", []string{"vm/machine.ovf", "vm/disks/disk.vmdk"}, false, true},
		{"truncated", "image.qcow2", []string{"image.qcow2"}, true, false},
		{"duplicate", "disk.vmdk", []string{"disk.vmdk", "disk.vmdk"}, false, false},
		{"escaped", "disk.vmdk", []string{"../disk.vmdk"}, false, false},
		{"missing main", "machine.ovf", []string{"disk.vmdk"}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := &Engine{cfg: Config{DataDir: t.TempDir()}, locks: make(map[string]*objectLock)}
			var input bytes.Buffer
			body := multipart.NewWriter(&input)
			for _, name := range test.files {
				part, err := body.CreateFormFile("files", name)
				if err != nil {
					t.Fatal(err)
				}
				io.WriteString(part, "image-content")
			}
			body.Close()
			raw := input.Bytes()
			if test.truncate {
				raw = raw[:len(raw)-len(body.Boundary())-10]
			}
			parts := multipart.NewReader(bytes.NewReader(raw), body.Boundary())
			path, err := e.ReceiveTemplate("test", 1, "upload", test.main, parts)
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
				if path != filepath.Join(importDirectory(e.cfg.DataDir, "test", 1), "upload", test.main) {
					t.Fatalf("wrong main path: %s", path)
				}
				for _, name := range test.files {
					data, err := os.ReadFile(filepath.Join(importDirectory(e.cfg.DataDir, "test", 1), "upload", name))
					if err != nil || string(data) != "image-content" {
						t.Fatalf("relative file %s: %v", name, err)
					}
				}
				if _, err = e.ReceiveTemplate("test", 1, "other-upload", test.main, multipart.NewReader(bytes.NewReader(raw), body.Boundary())); err != nil {
					t.Fatal(err)
				}
				if err = e.RemoveTemplateImport("test", 1, "upload"); err != nil {
					t.Fatal(err)
				}
				if _, err = os.Stat(filepath.Join(importDirectory(e.cfg.DataDir, "test", 1), "other-upload", test.main)); err != nil {
					t.Fatalf("cleanup removed another request: %v", err)
				}
				if err = e.RemoveTemplateImport("test", 1, "other-upload"); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid upload accepted")
			}
			entries, err := os.ReadDir(importDirectory(e.cfg.DataDir, "test", 1))
			if err != nil || len(entries) != 0 {
				t.Fatalf("incomplete files remain: %v %v", entries, err)
			}
		})
	}
}
