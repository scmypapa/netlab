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

func TestLogFollowAfterRotationRegrowth(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("logrotate uses the node's root identity")
	}
	directory := t.TempDir()
	writer, err := os.OpenFile(filepath.Join(directory, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	first := strings.Repeat("old", 32)
	if _, err = writer.WriteString(first); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(writer.Name())
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	watch := os.NewFile(uintptr(fd), "log-watch")
	if _, err = unix.InotifyAddWatch(fd, directory, unix.IN_MODIFY|unix.IN_CLOSE_WRITE); err != nil {
		watch.Close()
		t.Fatal(err)
	}
	reader := &LogReader{options: logfile.Options{Follow: true}, directory: directory, files: map[api.LogChunkStream]*os.File{api.LogChunkStreamStdout: file}, ends: map[api.LogChunkStream]int64{api.LogChunkStreamStdout: int64(len(first))}, watch: watch, closed: make(chan struct{})}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	paused, resume := make(chan struct{}), make(chan struct{})
	chunks, done := make(chan string, 4), make(chan error, 1)
	go func() {
		done <- reader.Read(ctx, func(chunk api.LogChunk) error {
			if chunk.Data == first {
				close(paused)
				select {
				case <-resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			} else {
				chunks <- chunk.Data
			}
			return nil
		})
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = writer.Truncate(16<<20 + 1); err != nil {
		t.Fatal(err)
	}
	if err = rotateLogFiles(ctx, directory, []string{directory}); err != nil {
		t.Fatal(err)
	}
	replacement := strings.Repeat("new", 64)
	if _, err = writer.WriteString(replacement); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case actual := <-chunks:
		if actual != replacement {
			t.Fatalf("rotation regrowth hid the new log prefix: got %d bytes, want %d", len(actual), len(replacement))
		}
	case <-ctx.Done():
		t.Fatal("new log was hidden behind the pre-rotation offset")
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLogFollowDeliversBothBusyStreams(t *testing.T) {
	directory := t.TempDir()
	stdout, err := os.OpenFile(filepath.Join(directory, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	block := strings.Repeat("out\n", 8192)
	if _, err = stdout.WriteString(block); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "stderr.log"), []byte("important error\n"), 0600); err != nil {
		t.Fatal(err)
	}
	files := map[api.LogChunkStream]*os.File{}
	for _, stream := range []api.LogChunkStream{api.LogChunkStreamStdout, api.LogChunkStreamStderr} {
		files[stream], err = os.Open(filepath.Join(directory, string(stream)+".log"))
		if err != nil {
			t.Fatal(err)
		}
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	watch := os.NewFile(uintptr(fd), "log-watch")
	if _, err = unix.InotifyAddWatch(fd, directory, unix.IN_MODIFY|unix.IN_CLOSE_WRITE); err != nil {
		watch.Close()
		t.Fatal(err)
	}
	reader := &LogReader{options: logfile.Options{Follow: true, Stream: "all"}, directory: directory, files: files, watch: watch, closed: make(chan struct{})}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outputs, errorsSeen := 0, 0
	err = reader.Read(ctx, func(chunk api.LogChunk) error {
		if chunk.Stream == api.LogChunkStreamStdout {
			outputs++
			if outputs > 4 {
				return errors.New("stdout prevented stderr delivery")
			}
			_, err := stdout.WriteString(block)
			return err
		}
		errorsSeen++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || errorsSeen != 1 || outputs != 1 {
		t.Fatalf("stderr was starved: stdout=%d stderr=%d error=%v", outputs, errorsSeen, err)
	}
}
