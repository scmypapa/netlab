//go:build linux

package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/platforms"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"netlab.local/core/api"
)

func declaredImageVolumes(config imagespec.Image) *[]string {
	paths := make([]string, 0, len(config.Config.Volumes))
	for path := range config.Config.Volumes {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return &paths
}

func containerTemplateMetadata(ctx context.Context, directory string, template api.Template) (api.Template, error) {
	if template.ImageVolumes != nil {
		return template, nil
	}
	config, err := archivedImageConfig(ctx, filepath.Join(directory, "image.tar"))
	if err != nil {
		return template, err
	}
	template.ImageVolumes = declaredImageVolumes(config)
	return template, nil
}

// OCI archives retain the original image metadata. Seek over layer bodies rather
// than importing and unpacking an entire image to upgrade its catalogue entry.
func archivedImageConfig(ctx context.Context, path string) (imagespec.Image, error) {
	var config imagespec.Image
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()
	archive := tar.NewReader(file)
	contents := archiveContents{}
	for {
		if err := ctx.Err(); err != nil {
			return config, err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return config, err
		}
		offset, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return config, err
		}
		contents[strings.TrimPrefix(header.Name, "./")] = io.NewSectionReader(file, offset, header.Size)
	}
	indexFile, exists := contents["index.json"]
	if !exists {
		return config, fmt.Errorf("OCI 归档缺少 index.json")
	}
	var index imagespec.Index
	if err := json.NewDecoder(indexFile).Decode(&index); err != nil {
		return config, err
	}
	if len(index.Manifests) != 1 {
		return config, fmt.Errorf("模板归档应包含一个镜像，实际为 %d", len(index.Manifests))
	}
	descriptor, err := images.Config(ctx, contents, index.Manifests[0], platforms.Default())
	if err != nil {
		return config, err
	}
	blob, err := content.ReadBlob(ctx, contents, descriptor)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(blob, &config)
	return config, err
}

type archiveContents map[string]*io.SectionReader

func (contents archiveContents) ReaderAt(_ context.Context, descriptor imagespec.Descriptor) (content.ReaderAt, error) {
	path := "blobs/" + descriptor.Digest.Algorithm().String() + "/" + descriptor.Digest.Encoded()
	reader, exists := contents[path]
	if !exists {
		return nil, fmt.Errorf("OCI 归档缺少 %s", path)
	}
	return archiveBlob{reader}, nil
}

type archiveBlob struct{ *io.SectionReader }

func (archiveBlob) Close() error { return nil }
