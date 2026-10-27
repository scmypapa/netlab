//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"netlab.local/core/api"
)

func (c *Containers) startLogRotation() {
	c.watches.Add(1)
	go func() {
		defer c.watches.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				if err := c.rotateLogs(); err != nil && c.ctx.Err() == nil {
					slog.Error("container log rotation", "error", err)
				}
			}
		}
	}()
}

func (c *Containers) rotateLogs() error {
	items, err := c.client.Containers(c.ctx, "labels.\""+nodeLabel+"\"=="+c.node)
	if err != nil {
		return err
	}
	directories := make([]string, 0, len(items))
	for _, item := range items {
		info, err := item.Info(c.ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			return err
		}
		if !managedContainer(info.Labels, c.node) {
			continue
		}
		var execution api.AssetExecution
		if err := json.Unmarshal([]byte(info.Labels[executionLabel]), &execution); err != nil {
			return err
		}
		directories = append(directories, assetDirectory(c.data, info.Labels[environmentLabel], execution))
	}
	return rotateLogFiles(c.ctx, c.data, directories)
}

func rotateLogFiles(ctx context.Context, directory string, directories []string) error {
	if len(directories) == 0 {
		return nil
	}
	var config strings.Builder
	for _, directory := range directories {
		fmt.Fprintf(&config, "%q %q\n", filepath.Join(directory, "stdout.log"), filepath.Join(directory, "stderr.log"))
	}
	// copytruncate keeps the shim's open inode; rotation never depends on a
	// per-container logger process or a second registry of storage locations.
	config.WriteString("{\nsize 16M\nrotate 1\ncopytruncate\nmissingok\nnotifempty\nsu root root\n}\n")
	file, err := os.CreateTemp(directory, ".logrotate-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(config.String())
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	// Rotation is size-based, so no second persistent state is necessary.
	command := exec.CommandContext(ctx, "logrotate", "--state", "/dev/null", file.Name())
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("logrotate: %w: %s", err, output)
	}
	return nil
}
