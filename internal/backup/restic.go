package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"netlab.local/core/api"
)

// Store delegates encryption, compression, deduplication and repository locking to restic.
type Store struct {
	Location, Password, CacheDirectory string
	AccessKey, SecretKey, Region       string
}

type Snapshot struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Tags []string  `json:"tags"`
}

func TemplateKey(t api.Template) string { return fmt.Sprintf("%s/%d", t.Id, t.Version) }

func (s Store) command(ctx context.Context, args ...string) *exec.Cmd {
	options := []string{"--quiet", "--json"}
	if s.CacheDirectory != "" {
		options = append(options, "--cache-dir", s.CacheDirectory)
	} else {
		options = append(options, "--no-cache")
	}
	command := exec.CommandContext(ctx, "restic", append(options, args...)...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "RESTIC_") && !strings.HasPrefix(value, "AWS_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "RESTIC_REPOSITORY="+s.Location, "RESTIC_PASSWORD="+s.Password,
		"AWS_ACCESS_KEY_ID="+s.AccessKey, "AWS_SECRET_ACCESS_KEY="+s.SecretKey, "AWS_DEFAULT_REGION="+s.Region)
	return command
}

func (s Store) run(ctx context.Context, input io.Reader, output io.Writer, args ...string) error {
	command := s.command(ctx, args...)
	var stderr bytes.Buffer
	command.Stdin, command.Stdout, command.Stderr = input, output, &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("restic %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s Store) Initialize(ctx context.Context) error {
	return s.run(ctx, nil, io.Discard, "init")
}

func (s Store) Connect(ctx context.Context, initialize bool) (string, error) {
	id, err := s.ID(ctx)
	var exit *exec.ExitError
	if err == nil || !initialize || !errors.As(err, &exit) || exit.ExitCode() != 10 {
		return id, err
	}
	if err = s.Initialize(ctx); err != nil {
		return "", err
	}
	return s.ID(ctx)
}

func (s Store) ID(ctx context.Context) (string, error) {
	var raw bytes.Buffer
	if err := s.run(ctx, nil, &raw, "cat", "config"); err != nil {
		return "", err
	}
	var config struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(raw.Bytes(), &config)
	return config.ID, err
}

func (s Store) Snapshots(ctx context.Context, tag string) ([]Snapshot, error) {
	var raw bytes.Buffer
	args := []string{"snapshots"}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	if err := s.run(ctx, nil, &raw, args...); err != nil {
		return nil, err
	}
	var snapshots []Snapshot
	err := json.Unmarshal(raw.Bytes(), &snapshots)
	return snapshots, err
}

// Put uses one stable operation/part tag. A published native snapshot can be reused after response loss.
func (s Store) Put(ctx context.Context, group, part, name string, size int64, reader io.Reader) (Snapshot, error) {
	tag := group + "/" + part
	previous, err := s.completeSnapshot(ctx, tag, name, size)
	if err != nil {
		return Snapshot{}, err
	}
	if previous.ID != "" {
		return previous, nil
	}
	input := &sizedReader{Reader: reader, remaining: size}
	if closer, ok := reader.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { closer.Close() })
		defer stop()
	}
	var raw bytes.Buffer
	if err = s.run(ctx, input, &raw, "backup", "--stdin", "--stdin-filename", name, "--host", "netlab", "--tag", group, "--tag", tag); err != nil {
		return Snapshot{}, err
	}
	var summary struct {
		ID   string    `json:"snapshot_id"`
		Time time.Time `json:"backup_end"`
	}
	if err = json.Unmarshal(raw.Bytes(), &summary); err != nil {
		return Snapshot{}, err
	}
	if summary.ID == "" || input.remaining != 0 {
		return Snapshot{}, errors.New("restic did not publish the complete backup stream")
	}
	return Snapshot{ID: summary.ID, Time: summary.Time}, nil
}

func (s Store) completeSnapshot(ctx context.Context, tag, name string, size int64) (Snapshot, error) {
	snapshots, err := s.Snapshots(ctx, tag)
	if err != nil {
		return Snapshot{}, err
	}
	for _, snapshot := range snapshots {
		complete, err := s.hasFile(ctx, snapshot.ID, name, size)
		if err != nil {
			return Snapshot{}, err
		}
		if complete {
			return snapshot, nil
		}
	}
	return Snapshot{}, nil
}

func (s Store) hasFile(ctx context.Context, snapshot, name string, size int64) (bool, error) {
	var raw bytes.Buffer
	if err := s.run(ctx, nil, &raw, "ls", "--", snapshot, name); err != nil {
		return false, err
	}
	decoder := json.NewDecoder(&raw)
	for {
		var entry struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		if entry.Type == "file" && entry.Path == name && entry.Size == size {
			return true, nil
		}
	}
}

func (s Store) Dump(ctx context.Context, snapshot, name string, writer io.Writer) error {
	return s.run(ctx, nil, writer, "dump", "--", snapshot, name)
}

func (s Store) Delete(ctx context.Context, group string) error {
	snapshots, err := s.Snapshots(ctx, group)
	if err != nil {
		return err
	}
	if len(snapshots) > 0 {
		args := []string{"forget", "--"}
		for _, snapshot := range snapshots {
			args = append(args, snapshot.ID)
		}
		if err = s.run(ctx, nil, io.Discard, args...); err != nil {
			return err
		}
	}
	// Prune still runs when a previous request forgot snapshots but lost its response.
	return s.run(ctx, nil, io.Discard, "prune")
}

type sizedReader struct {
	io.Reader
	remaining int64
}

func (r *sizedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, errors.New("backup stream exceeds its declared size")
	}
	if errors.Is(err, io.EOF) && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
