//go:build linux

package engine

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strconv"
)

func importDirectory(data, id string, version int) string {
	return filepath.Join(data, "imports", id, strconv.Itoa(version))
}

func (e *Engine) ReceiveTemplate(id string, version int, uploadID, source string, parts *multipart.Reader) (string, error) {
	unlock := e.lock("template:" + id)
	defer unlock()
	directory := filepath.Join(importDirectory(e.cfg.DataDir, id, version), uploadID)
	main, err := artifactPath(directory, source)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(directory), 0711); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Dir(directory), "upload-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err = os.Chmod(stage, 0711); err != nil {
		return "", err
	}
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		_, fields, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return "", err
		}
		if fields["name"] != "files" || fields["filename"] == "" {
			return "", errors.New("upload parts must be named files and include a filename")
		}
		path, err := artifactPath(stage, fields["filename"])
		if err != nil {
			return "", err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0711); err != nil {
			return "", err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(file, part)
		if err = errors.Join(copyErr, file.Close()); err != nil {
			return "", err
		}
	}
	path, _ := artifactPath(stage, source)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("main upload %s is not a file", source)
	}
	if err = os.Rename(stage, directory); err != nil {
		return "", err
	}
	return main, nil
}

func (e *Engine) RemoveTemplateImport(id string, version int, uploadID string) error {
	unlock := e.lock("template:" + id)
	defer unlock()
	return os.RemoveAll(filepath.Join(importDirectory(e.cfg.DataDir, id, version), uploadID))
}
