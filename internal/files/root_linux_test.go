//go:build linux

package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootFileLifecycle(t *testing.T) {
	rootDir := t.TempDir()
	store, err := OpenRoot(rootDir, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(store.Mkdir("/data"))
	must(store.Write(context.Background(), "/data/中文.txt", strings.NewReader("original")))
	must(os.Symlink("/data", filepath.Join(rootDir, "absolute")))
	reader, size, err := store.Read("/absolute/中文.txt")
	must(err)
	value, err := io.ReadAll(reader)
	must(err)
	must(reader.Close())
	if size != 8 || string(value) != "original" {
		t.Fatal("absolute container link did not resolve inside root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = store.Write(ctx, "/data/中文.txt", strings.NewReader("incomplete")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reader, _, err = store.Read("/data/中文.txt")
	must(err)
	value, err = io.ReadAll(reader)
	must(err)
	reader.Close()
	if string(value) != "original" {
		t.Fatal("interrupted upload replaced original")
	}
	entries, err := store.List("/data")
	must(err)
	if len(entries) != 1 || entries[0].Name != "中文.txt" {
		t.Fatal("upload temporary file remained", entries)
	}
	must(store.Rename("/data/中文.txt", "/data/renamed.txt"))
	must(os.Symlink("/", filepath.Join(rootDir, "root-alias")))
	if err = store.Remove("/root-alias/data/.."); err == nil {
		t.Fatal("dot-directory removal accepted")
	}
	must(store.Write(context.Background(), "/data/other.txt", strings.NewReader("other")))
	if err = store.Rename("/data/renamed.txt", "/data/other.txt"); !errors.Is(err, os.ErrExist) {
		t.Fatal("rename overwrote destination", err)
	}
	outside := t.TempDir()
	must(os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600))
	must(os.Symlink(outside, filepath.Join(rootDir, "data", "host-link")))
	must(store.Remove("/data"))
	if _, err = os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("recursive delete followed a symlink", err)
	}
	if _, _, err = store.Read("/../../etc/passwd"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("path escaped container root", err)
	}
	if err = store.Remove("/"); err == nil {
		t.Fatal("root removal accepted")
	}
}
