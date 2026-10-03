//go:build linux

package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"netlab.local/core/api"
)

func templateDirectory(data, id string, version int) string {
	return filepath.Join(data, "artifacts", id, strconv.Itoa(version))
}

func (e *Engine) OpenTemplateArtifact(id string, version int) (io.ReadCloser, int64, error) {
	directory := templateDirectory(e.cfg.DataDir, id, version)
	if _, err := os.Stat(filepath.Join(directory, "template.json")); err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, 0, err
	}
	headers := make([]*tar.Header, 0, len(entries))
	length := int64(1024)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, 0, err
		}
		if !info.Mode().IsRegular() {
			return nil, 0, fmt.Errorf("template artifact %s is not a regular file", entry.Name())
		}
		header := &tar.Header{Name: entry.Name(), Mode: 0640, Size: info.Size(), Typeflag: tar.TypeReg, Format: tar.FormatGNU}
		headers = append(headers, header)
		length += 512 + (header.Size+511)/512*512
	}
	reader, writer := io.Pipe()
	go func() {
		archive := tar.NewWriter(writer)
		var err error
		for _, header := range headers {
			if err = archive.WriteHeader(header); err != nil {
				break
			}
			var file *os.File
			file, err = os.Open(filepath.Join(directory, header.Name))
			if err != nil {
				break
			}
			_, copyErr := io.CopyN(archive, file, header.Size)
			err = errors.Join(copyErr, file.Close())
			if err != nil {
				break
			}
		}
		if err == nil {
			err = archive.Close()
		}
		writer.CloseWithError(err)
	}()
	return reader, length, nil
}

func (e *Engine) fetchTemplateArtifact(ctx context.Context, t api.Template, endpoint string) error {
	if t.ArtifactNodeId == nil || *t.ArtifactNodeId == e.cfg.ID {
		return nil
	}
	unlock := e.lock(fmt.Sprintf("artifact:%s:%d", t.Id, t.Version))
	defer unlock()
	directory := templateDirectory(e.cfg.DataDir, t.Id, t.Version)
	if _, err := os.Stat(filepath.Join(directory, "template.json")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if endpoint == "" || e.cfg.ArtifactHTTP == nil {
		return errors.New("template artifact node transport is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/node/v1/templates/%s/versions/%d/artifact", strings.TrimRight(endpoint, "/"), t.Id, t.Version), nil)
	if err != nil {
		return err
	}
	response, err := e.cfg.ArtifactHTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, err := io.ReadAll(io.LimitReader(response.Body, 16384))
		if err != nil {
			return err
		}
		return fmt.Errorf("template artifact download: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err = os.MkdirAll(filepath.Dir(directory), 0711); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(directory), "transfer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err = os.Chmod(staging, 0711); err != nil {
		return err
	}
	archive := tar.NewReader(response.Body)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		path, err := artifactPath(staging, header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != header.Name {
			return fmt.Errorf("invalid template artifact entry %q", header.Name)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, archive)
		if err = errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
	}
	// Consume the HTTP body so a truncated Content-Length cannot commit a cache entry.
	if _, err = io.Copy(io.Discard, response.Body); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(staging, "template.json"))
	if err != nil {
		return err
	}
	var prepared api.Template
	if err = json.Unmarshal(raw, &prepared); err != nil {
		return err
	}
	if prepared.Id != t.Id || prepared.Version != t.Version || prepared.Kind != t.Kind {
		return errors.New("template artifact identity does not match requested version")
	}
	files := []string{"image.tar"}
	if prepared.Kind == api.Vm {
		if prepared.Disks == nil || len(*prepared.Disks) == 0 {
			return errors.New("VM artifact contains no disks")
		}
		files = make([]string, len(*prepared.Disks))
		for index := range files {
			files[index] = fmt.Sprintf("disk-%d.qcow2", index)
		}
	}
	for _, name := range files {
		if _, err = os.Stat(filepath.Join(staging, name)); err != nil {
			return err
		}
	}
	return os.Rename(staging, directory)
}
