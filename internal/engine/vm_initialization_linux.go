//go:build linux

package engine

import (
	"context"
	"os"
	"path/filepath"

	"netlab.local/core/api"
)

// The generated ISO is staged until the caller has defined the VM successfully.
func stageInitialization(ctx context.Context, directory string, a api.AssetExecution) (string, error) {
	seed, err := initializationSeed(a)
	if err != nil || len(seed.files) == 0 {
		return "", err
	}
	staging, err := os.MkdirTemp(directory, "seed-")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(staging)
		}
	}()
	input := filepath.Join(staging, "input")
	for relative, content := range seed.files {
		path := filepath.Join(input, relative)
		if err = os.MkdirAll(filepath.Dir(path), 0711); err != nil {
			return "", err
		}
		if err = os.WriteFile(path, content, 0640); err != nil {
			return "", err
		}
	}
	output := filepath.Join(staging, "initialization.iso")
	if err = command(ctx, "genisoimage", "-quiet", "-rock", "-joliet", "-volid", seed.label, "-output", output, input); err != nil {
		return "", err
	}
	if err = os.Chmod(output, 0644); err != nil {
		return "", err
	}
	complete = true
	return output, nil
}
