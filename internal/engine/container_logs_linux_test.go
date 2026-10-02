//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/logfile"
)

func logJSON(chunk api.LogChunk) (api.LogChunk, error) {
	raw, err := json.Marshal(chunk)
	if err == nil {
		err = json.Unmarshal(raw, &chunk)
	}
	return chunk, err
}

func TestContainerLogUTF8Tail(t *testing.T) {
	for _, text := range []string{"中", "🙂"} {
		for split := 1; split < len(text); split++ {
			t.Run(text+string(rune('0'+split)), func(t *testing.T) {
				input := strings.Repeat("x", (32<<10)-split) + text + "end\n"
				path := filepath.Join(t.TempDir(), "stdout.log")
				if err := os.WriteFile(path, []byte(input), 0600); err != nil {
					t.Fatal(err)
				}
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				end, err := logfile.SeekTail(file, 1)
				if err != nil {
					t.Fatal(err)
				}
				reader := &LogReader{files: map[api.LogChunkStream]*os.File{api.LogChunkStreamStdout: file}, ends: map[api.LogChunkStream]int64{api.LogChunkStreamStdout: end}}
				defer reader.Close()
				var output strings.Builder
				err = reader.Read(context.Background(), func(chunk api.LogChunk) error {
					chunk, err := logJSON(chunk)
					output.WriteString(chunk.Data)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				if output.String() != input {
					t.Fatalf("JSON log chunks damaged UTF-8 at byte %d: tail=%q", (32<<10)-split, output.String()[(32<<10)-split:])
				}
			})
		}
	}
}

func TestContainerLogUTF8Follow(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	chunks, done := make(chan string, 4), make(chan error, 1)
	go func() {
		done <- reader.Read(ctx, func(chunk api.LogChunk) error {
			chunk, err := logJSON(chunk)
			chunks <- chunk.Data
			return err
		})
	}()
	if _, err = writer.Write([]byte("ready:\xe4")); err != nil {
		t.Fatal(err)
	}
	select {
	case first := <-chunks:
		if first != "ready:" {
			t.Fatalf("incomplete UTF-8 was emitted: %q", first)
		}
	case <-ctx.Done():
		t.Fatal("initial log chunk timed out")
	}
	if _, err = writer.Write([]byte("\xb8\xad文")); err != nil {
		t.Fatal(err)
	}
	select {
	case next := <-chunks:
		if next != "中文" {
			t.Fatalf("split write was damaged: %q", next)
		}
	case <-ctx.Done():
		t.Fatal("continued log chunk timed out")
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("follow cancellation: %v", err)
	}
}
