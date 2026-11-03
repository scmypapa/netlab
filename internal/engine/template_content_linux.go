//go:build linux

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"netlab.local/core/api"
)

// Only immutable artifacts are linked; running disks and recovery overlays keep their own files.
func (e *Engine) shareTemplateContent(ctx context.Context, directory string, t api.Template, runtimeOnly bool) error {
	if t.Kind != api.Vm {
		return nil
	}
	for _, name := range templateArtifactFiles(t, runtimeOnly) {
		if name == "template.json" {
			continue
		}
		if err := e.shareTemplateFile(ctx, filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) shareTemplateFile(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Sys().(*syscall.Stat_t).Nlink > 1 {
		return nil
	}
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := file.Read(buffer)
		hash.Write(buffer[:n])
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	unlock, err := e.lock(ctx, "template-content")
	if err != nil {
		return err
	}
	defer unlock()
	directory := filepath.Join(e.cfg.DataDir, "template-content")
	if err = os.MkdirAll(directory, 0711); err != nil {
		return err
	}
	object := filepath.Join(directory, hex.EncodeToString(hash.Sum(nil)))
	if err = os.Link(path, object); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	// Replace the directory entry atomically so open readers retain a complete file.
	link := path + ".shared"
	if err = os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	defer os.Remove(link)
	if err = os.Link(object, link); err != nil {
		return err
	}
	return os.Rename(link, path)
}

func (e *Engine) pruneTemplateContent(ctx context.Context) error {
	unlock, err := e.lock(ctx, "template-content")
	if err != nil {
		return err
	}
	defer unlock()
	directory := filepath.Join(e.cfg.DataDir, "template-content")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Sys().(*syscall.Stat_t).Nlink == 1 {
			if err = os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
