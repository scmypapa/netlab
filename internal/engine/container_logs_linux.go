//go:build linux

package engine

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/namespaces"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/logfile"
)

type containerLogIO struct{ config cio.Config }

func (l *containerLogIO) Config() cio.Config { return l.config }
func (l *containerLogIO) Cancel()            {}
func (l *containerLogIO) Wait()              {}
func (l *containerLogIO) Close() error       { return nil }

// Plain file destinations are copied by the runtime shim, so node restarts do
// not interrupt logging and stdout/stderr retain their own identity.
func containerLogFiles(directory string) cio.Creator {
	return func(string) (cio.IO, error) {
		config := cio.Config{Stdout: filepath.Join(directory, "stdout.log"), Stderr: filepath.Join(directory, "stderr.log")}
		for _, path := range []string{config.Stdout, config.Stderr} {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				return nil, err
			}
			if err = file.Close(); err != nil {
				return nil, err
			}
		}
		return &containerLogIO{config: config}, nil
	}
}

type LogReader struct {
	options   logfile.Options
	directory string
	files     map[api.LogChunkStream]*os.File
	ends      map[api.LogChunkStream]int64
	watch     *os.File
	closed    chan struct{}
}

func (e *Engine) OpenLogs(ctx context.Context, env, asset, instance string, options logfile.Options) (*LogReader, error) {
	if e.container == nil {
		return nil, errors.New("container runtime not configured")
	}
	ctx = namespaces.WithNamespace(ctx, "netlab")
	container, err := e.container.client.LoadContainer(ctx, instance)
	if err != nil {
		return nil, err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return nil, err
	}
	if !managedContainer(labels, e.cfg.ID) || labels[environmentLabel] != env || labels[assetLabel] != asset {
		return nil, errors.New("container ownership does not match log request")
	}
	var execution api.AssetExecution
	if err = json.Unmarshal([]byte(labels[executionLabel]), &execution); err != nil {
		return nil, err
	}
	directory := assetDirectory(e.cfg.DataDir, env, execution)
	reader := &LogReader{options: options, directory: directory, files: map[api.LogChunkStream]*os.File{}, ends: map[api.LogChunkStream]int64{}, closed: make(chan struct{})}
	if options.Follow {
		fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
		if err != nil {
			return nil, err
		}
		reader.watch = os.NewFile(uintptr(fd), "container-log-watch")
		if _, err = unix.InotifyAddWatch(fd, directory, unix.IN_MODIFY|unix.IN_CREATE|unix.IN_CLOSE_WRITE|unix.IN_DELETE_SELF); err != nil {
			reader.Close()
			return nil, err
		}
	}
	for _, stream := range []api.LogChunkStream{api.LogChunkStreamStdout, api.LogChunkStreamStderr} {
		if options.Stream != "all" && options.Stream != string(stream) {
			continue
		}
		file, err := os.Open(filepath.Join(directory, string(stream)+".log"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			reader.Close()
			return nil, err
		}
		reader.files[stream] = file
		if reader.ends[stream], err = logfile.SeekTail(file, options.Tail); err != nil {
			reader.Close()
			return nil, err
		}
	}
	return reader, nil
}

func (r *LogReader) Close() error {
	var errs []error
	for _, file := range r.files {
		errs = append(errs, file.Close())
	}
	if r.watch != nil {
		errs = append(errs, r.watch.Close())
	}
	return errors.Join(errs...)
}

func (r *LogReader) readEvents(ctx context.Context, buffer []byte, pending map[api.LogChunkStream][]byte, wait bool) error {
	if r.watch == nil {
		return nil
	}
	raw, err := r.watch.SyscallConn()
	if err != nil {
		return err
	}
	for {
		var n int
		if wait {
			n, err = r.watch.Read(buffer)
			wait = false
		} else {
			var readErr error
			err = raw.Control(func(fd uintptr) { n, readErr = unix.Read(int(fd), buffer) })
			if err == nil {
				err = readErr
			}
		}
		if errors.Is(err, unix.EAGAIN) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for offset := 0; offset < n; {
			mask := binary.NativeEndian.Uint32(buffer[offset+4 : offset+8])
			length := int(binary.NativeEndian.Uint32(buffer[offset+12 : offset+16]))
			name, archived := strings.CutSuffix(strings.TrimRight(string(buffer[offset+16:offset+16+length]), "\x00"), ".log.1")
			offset += 16 + length
			// The archive closes after copytruncate, even if the live file has already regrown.
			if mask&unix.IN_CLOSE_WRITE == 0 || !archived {
				continue
			}
			stream := api.LogChunkStream(name)
			if file := r.files[stream]; file != nil {
				if _, err = file.Seek(0, io.SeekStart); err != nil {
					return err
				}
				delete(r.ends, stream)
				delete(pending, stream)
			}
		}
	}
}

func (r *LogReader) Read(ctx context.Context, emit func(api.LogChunk) error) error {
	if r.watch != nil {
		go func() {
			select {
			case <-ctx.Done():
				r.watch.Close()
			case <-r.closed:
			}
		}()
		defer close(r.closed)
	}
	buffer := make([]byte, 32<<10)
	pending := map[api.LogChunkStream][]byte{}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progress := false
		// A busy stdout must not delay stderr or prevent processing rotation events.
		for _, stream := range []api.LogChunkStream{api.LogChunkStreamStdout, api.LogChunkStreamStderr} {
			if err := r.readEvents(ctx, buffer, pending, false); err != nil {
				return err
			}
			file := r.files[stream]
			if file == nil {
				if r.options.Stream != "all" && r.options.Stream != string(stream) {
					continue
				}
				var err error
				file, err = os.Open(filepath.Join(r.directory, string(stream)+".log"))
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				r.files[stream] = file
			}
			position, err := file.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if info.Size() < position {
				if position, err = file.Seek(0, io.SeekStart); err != nil {
					return err
				}
				delete(r.ends, stream)
				delete(pending, stream)
			}
			end := info.Size()
			if initial, ok := r.ends[stream]; ok {
				end = initial
			}
			prefix := len(pending[stream])
			copy(buffer, pending[stream])
			n, err := io.LimitReader(file, max(0, end-position)).Read(buffer[prefix:])
			position += int64(n)
			if n > 0 {
				progress = true
				n += prefix
				complete := n
				// A file write or read boundary can split a UTF-8 character.
				for start := n - 1; start >= max(0, n-utf8.UTFMax); start-- {
					if utf8.RuneStart(buffer[start]) {
						if !utf8.FullRune(buffer[start:n]) {
							complete = start
						}
						break
					}
				}
				pending[stream] = append(pending[stream][:0], buffer[complete:n]...)
				if complete > 0 {
					if err := emit(api.LogChunk{Stream: stream, Data: string(buffer[:complete])}); err != nil {
						return err
					}
				}
			}
			if err != nil && err != io.EOF {
				return err
			}
			if position >= end {
				if _, initial := r.ends[stream]; initial && r.options.Follow {
					delete(r.ends, stream)
					progress = true
				}
				if !r.options.Follow && len(pending[stream]) > 0 {
					if err := emit(api.LogChunk{Stream: stream, Data: string(pending[stream])}); err != nil {
						return err
					}
					delete(pending, stream)
				}
			}
		}
		if progress {
			continue
		}
		if !r.options.Follow {
			return nil
		}
		if _, err := os.Stat(r.directory); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if err := r.readEvents(ctx, buffer, pending, true); err != nil {
			return err
		}
	}
}
