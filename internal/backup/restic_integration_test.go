package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResticRepositoryLifecycle(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
	ctx := context.Background()
	t.Setenv("RESTIC_PASSWORD_FILE", "/nonexistent-ambient-password-file")
	s := Store{Location: filepath.Join(t.TempDir(), "repository"), Password: "native-test-password"}
	if _, err := s.Connect(ctx, true); err != nil {
		t.Fatal(err)
	}
	id, err := s.ID(ctx)
	if err != nil || len(id) != 64 {
		t.Fatalf("repository identity: %q %v", id, err)
	}
	if repeated, err := s.Connect(ctx, true); err != nil || repeated != id {
		t.Fatalf("repository connection replay: %q %v", repeated, err)
	}
	missing := Store{Location: filepath.Join(t.TempDir(), "missing"), Password: s.Password}
	if _, err := missing.Connect(ctx, false); err == nil {
		t.Fatal("uninitialized repository was accepted")
	}
	wrong := s
	wrong.Password = "wrong-native-test-password"
	if _, err = wrong.Connect(ctx, true); err == nil || strings.Contains(err.Error(), wrong.Password) {
		t.Fatalf("password rejection: %v", err)
	}
	content := bytes.Repeat([]byte("persistent environment data\n"), 1<<15)
	var captured Snapshot
	t.Run("stream and replay", func(t *testing.T) {
		var err error
		captured, err = s.Put(ctx, "netlab:backup:first", "asset", "/asset.tar", int64(len(content)), bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		if len(captured.ID) != 64 {
			t.Fatal("snapshot identity is not the full native ID:", captured.ID)
		}
		replayed, err := s.Put(ctx, "netlab:backup:first", "asset", "/asset.tar", int64(len(content)), errorReader{})
		if err != nil || replayed.ID != captured.ID {
			t.Fatalf("response-loss replay: %+v %v", replayed, err)
		}
		var restored bytes.Buffer
		if err = s.Dump(ctx, captured.ID, "/asset.tar", &restored); err != nil || !bytes.Equal(restored.Bytes(), content) {
			t.Fatalf("restored data differs: %v", err)
		}
	})
	t.Run("failed and truncated input", func(t *testing.T) {
		for _, reader := range []io.Reader{errorReader{}, bytes.NewReader(content[:len(content)/2]), bytes.NewReader(append(append([]byte{}, content...), 1))} {
			if _, err := s.Put(ctx, "netlab:backup:failed", "asset", "/asset.tar", int64(len(content)), reader); err == nil {
				t.Fatal("failed input was accepted")
			}
		}
		if _, err := s.Put(ctx, "netlab:backup:failed", "asset", "/asset.tar", int64(len(content)), bytes.NewReader(content)); err != nil {
			t.Fatal("retry after input failure:", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Put(canceled, "netlab:backup:canceled", "asset", "/asset.tar", int64(len(content)), bytes.NewReader(content)); err == nil {
			t.Fatal("canceled backup was accepted")
		}
		active, stop := context.WithCancel(ctx)
		defer stop()
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		if _, err := s.Put(active, "netlab:backup:canceled", "asset", "/asset.tar", int64(len(content)), cancelReader{reader, stop}); err == nil {
			t.Fatal("active transfer cancellation was accepted")
		}
	})
	t.Run("delete exact group and retry", func(t *testing.T) {
		if err := s.Delete(ctx, "netlab:backup:failed"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "netlab:backup:failed"); err != nil {
			t.Fatal("repeated deletion:", err)
		}
		remaining, err := s.Snapshots(ctx, "netlab:backup:failed")
		if err != nil || len(remaining) != 0 {
			t.Fatalf("deleted group remains: %+v %v", remaining, err)
		}
		var restored bytes.Buffer
		if err = s.Dump(ctx, captured.ID, "/asset.tar", &restored); err != nil || !bytes.Equal(restored.Bytes(), content) {
			t.Fatalf("deleting one group damaged another: %v", err)
		}
	})
	if err := s.Delete(ctx, "netlab:backup:first"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Location, "config")); err != nil {
		t.Fatal("deletion removed the repository:", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("source transfer failed") }

type cancelReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	r.cancel()
	return r.PipeReader.Read(p)
}
