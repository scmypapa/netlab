//go:build linux

package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/logfile"
)

func TestRealContainerLogRotation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("logrotate uses the node's root identity")
	}
	if _, err := exec.LookPath("logrotate"); err != nil {
		t.Fatal("node runtime dependency logrotate is missing")
	}
	root := t.TempDir()
	directories := []string{filepath.Join(root, "local pool"), filepath.Join(root, "other pool")}
	var writers []*os.File
	for _, directory := range directories {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		for _, stream := range []string{"stdout", "stderr"} {
			writer, err := os.OpenFile(filepath.Join(directory, stream+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if err := writer.Truncate(16<<20 + 1); err != nil {
				t.Fatal(err)
			}
			writers = append(writers, writer)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for round := range 2 {
		if err := rotateLogFiles(ctx, root, directories); err != nil {
			t.Fatal(err)
		}
		for _, writer := range writers {
			info, err := writer.Stat()
			if err != nil || info.Size() != 0 {
				t.Fatalf("round %d: open shim log was not truncated: %v %v", round, info, err)
			}
			current, err := os.Stat(writer.Name())
			if err != nil || !os.SameFile(info, current) {
				t.Fatalf("shim inode changed: %v", err)
			}
			if _, err := os.Stat(writer.Name() + ".1"); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.WriteString("continued\n"); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(writer.Name()); err != nil || string(data) != "continued\n" {
				t.Fatalf("shim could not continue writing: %q %v", data, err)
			}
			if err := writer.Truncate(16<<20 + 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, writer := range writers {
		if _, err := os.Stat(writer.Name() + ".2"); !os.IsNotExist(err) {
			t.Fatalf("rotation retained extra generations: %v", err)
		}
	}
	files, err := filepath.Glob(filepath.Join(root, ".logrotate-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary configuration retained: %v %v", files, err)
	}
}

func TestLogFollowAfterCopyTruncate(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "stdout.log")
	writer, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	watch := os.NewFile(uintptr(fd), "log-watch")
	if _, err = unix.InotifyAddWatch(fd, directory, unix.IN_MODIFY); err != nil {
		watch.Close()
		t.Fatal(err)
	}
	reader := &LogReader{options: logfile.Options{Follow: true}, directory: directory, files: map[api.LogChunkStream]*os.File{api.LogChunkStreamStdout: file}, watch: watch, closed: make(chan struct{})}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	chunks, done := make(chan string, 4), make(chan error, 1)
	go func() {
		done <- reader.Read(ctx, func(chunk api.LogChunk) error { chunks <- chunk.Data; return nil })
	}()
	first := strings.Repeat("old", 32)
	if _, err := writer.WriteString(first + "\xe4"); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-chunks:
		if text != first {
			t.Fatalf("initial log: %q", text)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := writer.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("new\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-chunks:
		if text != "new\n" {
			t.Fatalf("truncation retained old offset or UTF-8 fragment: %q", text)
		}
	case <-ctx.Done():
		t.Fatal("new log was hidden behind old offset")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}
