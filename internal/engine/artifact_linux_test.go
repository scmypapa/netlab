//go:build linux

package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"netlab.local/core/api"
)

func TestTemplateArtifactTransfer(t *testing.T) {
	origin := "source-node"
	disks := []api.TemplateDisk{{Id: "boot"}}
	template := api.Template{Id: "system", Version: 3, Kind: api.Vm, ArtifactNodeId: &origin, Disks: &disks}
	source := &Engine{cfg: Config{ID: origin, DataDir: t.TempDir()}}
	directory := templateDirectory(source.cfg.DataDir, template.Id, template.Version)
	if err := os.MkdirAll(directory, 0711); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("fixed disk contents\n"), 8192)
	for name, data := range map[string][]byte{"template.json": manifest, "disk-0.qcow2": content} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0640); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/node/v1/templates/system/versions/3/artifact" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		reader, length, err := source.OpenTemplateArtifact(template.Id, template.Version)
		if err != nil {
			t.Error(err)
			http.Error(w, err.Error(), 500)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		if _, err = io.Copy(w, reader); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	target := &Engine{cfg: Config{ID: "target-node", DataDir: t.TempDir(), ArtifactHTTP: server.Client()}, locks: make(map[string]*objectLock)}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := target.fetchTemplateArtifact(context.Background(), template, server.URL); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent assets downloaded %d copies", calls.Load())
	}
	server.Close()
	if err := target.fetchTemplateArtifact(context.Background(), template, server.URL); err != nil {
		t.Fatalf("cached artifact depends on online origin: %v", err)
	}
	actual, err := os.ReadFile(filepath.Join(templateDirectory(target.cfg.DataDir, template.Id, template.Version), "disk-0.qcow2"))
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("disk contents changed: %v", err)
	}
}

func TestTemplateArtifactRejectsInvalidTransfer(t *testing.T) {
	for _, kind := range []string{"truncated", "incomplete-stream", "wrong-version", "escape", "symlink", "missing-disk"} {
		t.Run(kind, func(t *testing.T) {
			origin := "source-node"
			disks := []api.TemplateDisk{{Id: "boot"}}
			template := api.Template{Id: "system", Version: 3, Kind: api.Vm, ArtifactNodeId: &origin, Disks: &disks}
			manifest := template
			if kind == "wrong-version" {
				manifest.Version++
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			var buffer bytes.Buffer
			archive := tar.NewWriter(&buffer)
			header := &tar.Header{Name: "template.json", Mode: 0640, Size: int64(len(raw)), Typeflag: tar.TypeReg}
			if kind == "escape" {
				header.Name = "../outside"
			}
			if kind == "symlink" {
				header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, "/tmp/outside", 0
			}
			if err = archive.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Size != 0 {
				if _, err = archive.Write(raw); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "incomplete-stream" {
				if err = archive.WriteHeader(&tar.Header{Name: "disk-0.qcow2", Mode: 0640, Size: 0, Typeflag: tar.TypeReg}); err != nil {
					t.Fatal(err)
				}
			}
			if err = archive.Close(); err != nil {
				t.Fatal(err)
			}
			body := buffer.Bytes()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				if kind == "incomplete-stream" {
					w.Header().Set("Content-Length", fmt.Sprint(len(body)+1))
				}
				if kind == "truncated" {
					body = body[:len(body)-1]
				}
				w.Write(body)
			}))
			defer server.Close()
			target := &Engine{cfg: Config{ID: "target-node", DataDir: t.TempDir(), ArtifactHTTP: server.Client()}, locks: make(map[string]*objectLock)}
			if err = target.fetchTemplateArtifact(context.Background(), template, server.URL); err == nil {
				t.Fatal("invalid transfer was committed")
			}
			entries, err := os.ReadDir(filepath.Join(target.cfg.DataDir, "artifacts", template.Id))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed transfer left cached data: %v %v", entries, err)
			}
		})
	}
}

func TestLargeTemplateArtifactHeader(t *testing.T) {
	source := &Engine{cfg: Config{DataDir: t.TempDir()}}
	directory := templateDirectory(source.cfg.DataDir, "large-disk", 1)
	if err := os.MkdirAll(directory, 0711); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "template.json"), []byte(`{"id":"large-disk","version":1,"kind":"vm"}`), 0640); err != nil {
		t.Fatal(err)
	}
	disk, err := os.Create(filepath.Join(directory, "disk-0.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	if err = disk.Truncate(9 << 30); err != nil {
		disk.Close()
		t.Fatal(err)
	}
	if err = disk.Close(); err != nil {
		t.Fatal(err)
	}
	reader, length, err := source.OpenTemplateArtifact("large-disk", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	header, err := tar.NewReader(reader).Next()
	if err != nil || header.Size != 9<<30 || length <= header.Size {
		t.Fatalf("large disk header was not representable: %v %v", header, err)
	}
}
