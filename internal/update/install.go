package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"netlab.local/core/api"
)

type Manifest struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Execute runs in its own systemd service, so controller restarts do not interrupt installation.
func Execute(ctx context.Context, cfg Config, listen string) (failure error) {
	directory := filepath.Join(cfg.DataDir, "update")
	requestPath := filepath.Join(directory, "request.json")
	data, err := os.ReadFile(requestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var request struct{ Version string }
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}
	defer func() {
		phase := api.UpdateActivityPhase("succeeded")
		if failure != nil {
			phase = "failed"
		}
		failure = errors.Join(failure, writeActivity(directory, request.Version, phase, failure), os.Remove(requestPath))
	}()
	if cfg.InstallDir == "" || runtime.GOOS != "linux" {
		return ErrNotInstalled
	}
	if _, err := versionParts(request.Version); err != nil {
		return err
	}
	releases := filepath.Join(cfg.InstallDir, "releases")
	destination := filepath.Join(releases, request.Version)
	if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
		if err := prepare(ctx, cfg, directory, destination); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := validate(destination, request.Version); err != nil {
		return err
	}
	current, err := os.Readlink(filepath.Join(cfg.InstallDir, "current"))
	if err != nil {
		return err
	}
	previous := current
	if filepath.Clean(current) != filepath.Clean(destination) {
		if err := switchLink(cfg.InstallDir, "previous", current); err != nil {
			return err
		}
		if err := switchLink(cfg.InstallDir, "current", destination); err != nil {
			return err
		}
	} else {
		previous, err = os.Readlink(filepath.Join(cfg.InstallDir, "previous"))
		if err != nil {
			return err
		}
	}
	if err := writeActivity(directory, request.Version, "restarting", nil); err != nil {
		return err
	}
	if err := restart(ctx, listen); err != nil {
		// Restore programs only. Database migrations remain forward-only.
		if rollbackErr := switchLink(cfg.InstallDir, "current", previous); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return errors.Join(err, restart(ctx, listen))
	}
	return nil
}

func prepare(ctx context.Context, cfg Config, directory, destination string) error {
	version := filepath.Base(destination)
	g := newGitHub(cfg)
	r, err := g.release(ctx, version)
	if err != nil {
		return err
	}
	if r.Tag != version {
		return fmt.Errorf("发布包版本与请求不一致")
	}
	name := "netlab_linux_" + runtime.GOARCH + ".tar.gz"
	var selected *artifact
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			selected = &r.Assets[i]
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("%s 缺少 %s", r.Tag, name)
	}
	if err := writeActivity(directory, version, "downloading", nil); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	archive := filepath.Join(staging, "package.tar.gz")
	if err := g.download(ctx, *selected, archive); err != nil {
		return err
	}
	if err := writeActivity(directory, version, "installing", nil); err != nil {
		return err
	}
	if err := extract(archive, staging); err != nil {
		return err
	}
	if err := os.Remove(archive); err != nil {
		return err
	}
	if err := validate(staging, version); err != nil {
		return err
	}
	if err := os.Chmod(staging, 0755); err != nil {
		return err
	}
	return os.Rename(staging, destination)
}

func validate(directory, version string) error {
	data, err := os.ReadFile(filepath.Join(directory, "release.json"))
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if manifest.Version != version || manifest.OS != "linux" || manifest.Arch != runtime.GOARCH {
		return fmt.Errorf("发布包版本或运行架构不匹配")
	}
	for _, name := range []string{"netlab-controller", "netlab-node", "web/index.html"} {
		file, err := os.Stat(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		if !file.Mode().IsRegular() {
			return fmt.Errorf("发布包缺少 %s", name)
		}
	}
	return nil
}

func switchLink(directory, name, target string) error {
	file, err := os.CreateTemp(directory, ".link-")
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return err
	}
	if err := os.Remove(file.Name()); err != nil {
		return err
	}
	if err := os.Symlink(target, file.Name()); err != nil {
		return err
	}
	defer os.Remove(file.Name())
	return os.Rename(file.Name(), filepath.Join(directory, name))
}

func restart(ctx context.Context, listen string) error {
	commands := [][]string{{"restart", "netlab-controller.service"}}
	if _, err := os.Stat("/etc/systemd/system/netlab-node.service"); err == nil {
		commands = append([][]string{{"try-restart", "netlab-node.service"}}, commands...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, args := range commands {
		if output, err := exec.CommandContext(ctx, "/usr/bin/systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("切换服务：%w：%s", err, output)
		}
	}
	return waitController(ctx, listen)
}

func (g github) download(ctx context.Context, asset artifact, destination string) error {
	const maxSize = 2 << 30
	digest, err := hex.DecodeString(strings.TrimPrefix(asset.Digest, "sha256:"))
	if err != nil || !strings.HasPrefix(asset.Digest, "sha256:") || len(digest) != sha256.Size {
		return fmt.Errorf("发布包缺少有效的完整性摘要")
	}
	if asset.Size <= 0 || asset.Size > maxSize {
		return fmt.Errorf("发布包大小超出 2 GiB")
	}
	if !strings.HasPrefix(asset.URL, g.baseURL+"/repos/"+g.cfg.Repository+"/releases/assets/") {
		return fmt.Errorf("发布包不属于配置的仓库")
	}
	response, err := g.get(ctx, asset.URL, "application/octet-stream")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, asset.Size+1))
	err = errors.Join(copyErr, file.Close())
	if err != nil {
		return err
	}
	if n != asset.Size || hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(digest) {
		return fmt.Errorf("发布包大小或完整性校验失败")
	}
	return nil
}

func extract(archive, destination string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	var expanded int64
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) || filepath.Clean(name) == "package.tar.gz" {
			return fmt.Errorf("发布包路径无效：%s", h.Name)
		}
		target := filepath.Join(destination, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			expanded += h.Size
			if expanded > 4<<30 {
				return fmt.Errorf("发布包展开后超出 4 GiB")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			mode := os.FileMode(0644)
			if h.Mode&0111 != 0 {
				mode = 0755
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, reader)
			if err := errors.Join(copyErr, output.Close()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("发布包包含非普通文件：%s", h.Name)
		}
	}
}

func waitController(ctx context.Context, listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		r, err := http.NewRequestWithContext(ctx, "GET", "http://"+net.JoinHostPort(host, port)+"/api/v1/identity", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(r)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("新版本控制面未启动：%w", ctx.Err())
		case <-timer.C:
		}
	}
}
