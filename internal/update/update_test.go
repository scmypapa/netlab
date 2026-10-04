package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReleaseVersions(t *testing.T) {
	for _, tc := range []struct {
		next, current string
		want          bool
	}{
		{"v1.10.0", "v1.9.9", true}, {"v1.0.1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0", false}, {"v0.9.0", "v1.0.0", false},
		{"v2.0.0", "dev", true}, {"v2.0.0-rc.1", "v1.0.0", false},
		{"v01.2.0", "v1.0.0", false},
	} {
		if got := newer(tc.next, tc.current); got != tc.want {
			t.Errorf("newer(%q,%q)=%v", tc.next, tc.current, got)
		}
	}
}

func TestCheckAndApply(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/example/netlab/releases/latest" || r.Header.Get("Authorization") != "Bearer private-token" {
			t.Errorf("wrong release request")
		}
		json.NewEncoder(w).Encode(release{Tag: "v1.2.0", Name: "Netlab 1.2.0", Body: "## Changes\n\n- Network improvements", PublishedAt: time.Now()})
	}))
	defer upstream.Close()
	cfg := Config{Repository: "example/netlab", Token: "private-token", Version: "v1.1.0", DataDir: t.TempDir()}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.github.baseURL = upstream.URL
	if err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(context.Background())
	if err != nil || !status.Available || status.Latest.Notes == "" || status.CanApply {
		t.Fatalf("%+v %v", status, err)
	}
	if err := s.Apply(context.Background(), "v1.2.0"); !errors.Is(err, ErrNotInstalled) {
		t.Fatal(err)
	}
	// Only the external service start is replaced; request ownership and durable progress use real files.
	s.cfg.InstallDir = t.TempDir()
	s.start = func(context.Context) error { return nil }
	if err := s.Apply(context.Background(), "v1.3.0"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), "v1.2.0"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	status, err = s.Status(context.Background())
	if err != nil || status.Activity.Phase != "queued" || status.Activity.Version != "v1.2.0" {
		t.Fatalf("%+v %v", status, err)
	}
	other, _ := New(cfg)
	other.cfg.InstallDir = s.cfg.InstallDir
	status, err = other.Status(context.Background())
	if err != nil || status.Activity.Phase != "queued" {
		t.Fatalf("durable progress missing: %+v %v", status, err)
	}
}

func TestApplyStartFailure(t *testing.T) {
	s, _ := New(Config{Repository: "example/netlab", Version: "v1.0.0", DataDir: t.TempDir()})
	r := release{Tag: "v1.1.0"}.public()
	s.latest = &r
	s.cfg.InstallDir = t.TempDir()
	s.start = func(context.Context) error { return errors.New("update unit unavailable") }
	if err := s.Apply(context.Background(), r.Version); err == nil {
		t.Fatal("accepted failed start")
	}
	status, err := s.Status(context.Background())
	if err != nil || status.Activity.Phase != "failed" || status.Activity.Error == nil {
		t.Fatalf("%+v %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(s.directory(), "request.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed request retained")
	}
}

func TestDownloadIntegrity(t *testing.T) {
	data := []byte("a real downloaded byte stream")
	hash := sha256.Sum256(data)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/octet-stream" {
			t.Error("wrong content type")
		}
		w.Write(data)
	}))
	defer upstream.Close()
	g := newGitHub(Config{Repository: "example/netlab"})
	g.baseURL = upstream.URL
	asset := artifact{URL: upstream.URL + "/repos/example/netlab/releases/assets/1", Size: int64(len(data)), Digest: "sha256:" + hex.EncodeToString(hash[:])}
	if err := g.download(context.Background(), asset, filepath.Join(t.TempDir(), "bundle")); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []artifact{
		{URL: asset.URL, Size: asset.Size - 1, Digest: asset.Digest},
		{URL: asset.URL, Size: asset.Size, Digest: "sha256:" + hex.EncodeToString(make([]byte, 32))},
		{URL: asset.URL, Size: asset.Size},
		{URL: upstream.URL + "/unrelated", Size: asset.Size, Digest: asset.Digest},
	} {
		if err := g.download(context.Background(), changed, filepath.Join(t.TempDir(), "bundle")); err == nil {
			t.Fatal("accepted changed artifact")
		}
	}
}

func TestArchiveExtraction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  byte
		valid bool
	}{
		{"web/index.html", tar.TypeReg, true},
		{"../escape", tar.TypeReg, false},
		{"/escape", tar.TypeReg, false},
		{"web/link", tar.TypeSymlink, false},
		{"package.tar.gz", tar.TypeReg, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			gz := gzip.NewWriter(&data)
			tw := tar.NewWriter(gz)
			h := &tar.Header{Name: tc.name, Mode: 0644, Typeflag: tc.kind}
			if tc.kind == tar.TypeReg {
				h.Size = 2
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if tc.kind == tar.TypeReg {
				tw.Write([]byte("ok"))
			}
			tw.Close()
			gz.Close()
			directory := t.TempDir()
			archive := filepath.Join(directory, "package.tar.gz")
			if err := os.WriteFile(archive, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			err := extract(archive, directory)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestInstallationConfiguration(t *testing.T) {
	if _, err := New(Config{Repository: "../invalid"}); err == nil {
		t.Fatal("accepted invalid repository")
	}
	if _, err := New(Config{Repository: "example/netlab", InstallDir: "relative"}); err == nil {
		t.Fatal("accepted relative installation")
	}
	if runtime.GOOS != "linux" {
		if _, err := New(Config{Repository: "example/netlab", InstallDir: t.TempDir()}); err == nil {
			t.Fatal("accepted non-Linux installation")
		}
	}
}

func TestReleaseDirectoryAndSwitch(t *testing.T) {
	directory := t.TempDir()
	for _, version := range []string{"v1.0.0", "v1.1.0"} {
		releaseDir := filepath.Join(directory, "releases", version)
		if err := os.MkdirAll(filepath.Join(releaseDir, "web"), 0755); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(Manifest{Version: version, OS: "linux", Arch: runtime.GOARCH})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(releaseDir, "release.json"), data, 0644); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"netlab-controller", "netlab-node", "web/index.html", "victoria-metrics-prod", "guacamole/sbin/guacd", "guacamole/lib/libguac-client-rdp.so"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(releaseDir, name)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(releaseDir, name), []byte("package fixture"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := validate(releaseDir, version); err != nil {
			t.Fatal(err)
		}
		if err := validate(releaseDir, "v9.0.0"); err == nil {
			t.Fatal("accepted wrong manifest")
		}
	}
	if runtime.GOOS != "linux" {
		t.Skip("atomic release symlink verified on Linux")
	}
	old := filepath.Join(directory, "releases", "v1.0.0")
	next := filepath.Join(directory, "releases", "v1.1.0")
	if err := switchLink(directory, "current", old); err != nil {
		t.Fatal(err)
	}
	if err := switchLink(directory, "previous", old); err != nil {
		t.Fatal(err)
	}
	if err := switchLink(directory, "current", next); err != nil {
		t.Fatal(err)
	}
	if current, err := os.Readlink(filepath.Join(directory, "current")); err != nil || current != next {
		t.Fatalf("%s %v", current, err)
	}
	if previous, err := os.Readlink(filepath.Join(directory, "previous")); err != nil || previous != old {
		t.Fatalf("%s %v", previous, err)
	}
	if err := switchLink(directory, "current", old); err != nil {
		t.Fatal(err)
	}
	if current, err := os.Readlink(filepath.Join(directory, "current")); err != nil || current != old {
		t.Fatalf("rollback: %s %v", current, err)
	}
}
