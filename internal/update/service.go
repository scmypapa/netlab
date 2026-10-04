package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"netlab.local/core/api"
)

var ErrConflict = errors.New("已有更新正在执行，或所选发布版本已变化")
var ErrNotInstalled = errors.New("源码开发环境不执行自更新")

type Service struct {
	cfg        Config
	github     github
	mu         sync.Mutex
	latest     *api.UpdateRelease
	checkedAt  *time.Time
	checkError *string
	start      func(context.Context) error
}

func New(cfg Config) (*Service, error) {
	if !repositoryPattern.MatchString(cfg.Repository) {
		return nil, fmt.Errorf("发布仓库应为 owner/repository")
	}
	if cfg.InstallDir != "" && (!filepath.IsAbs(cfg.InstallDir) || runtime.GOOS != "linux") {
		return nil, fmt.Errorf("自动更新需要 Linux 正式安装目录")
	}
	s := &Service{cfg: cfg, github: newGitHub(cfg)}
	s.start = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "sudo", "-n", "/usr/bin/systemctl", "start", "--no-block", "netlab-update.service").CombinedOutput()
		if err != nil {
			return fmt.Errorf("启动更新服务：%w：%s", err, output)
		}
		return nil
	}
	return s, nil
}

func (s *Service) Run(ctx context.Context) {
	timer := time.NewTicker(6 * time.Hour)
	defer timer.Stop()
	for {
		if err := s.Check(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("release check failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (s *Service) Check(ctx context.Context) error {
	r, err := s.github.release(ctx, "")
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.checkedAt = &now
	if err != nil {
		detail := err.Error()
		s.checkError = &detail
		return err
	}
	latest := r.public()
	s.latest = &latest
	s.checkError = nil
	return nil
}

func (s *Service) directory() string { return filepath.Join(s.cfg.DataDir, "update") }

func (s *Service) Status() (api.SystemUpdate, error) {
	s.mu.Lock()
	value := api.SystemUpdate{CurrentVersion: s.cfg.Version, Latest: s.latest, CheckedAt: s.checkedAt, CheckError: s.checkError, CanApply: s.cfg.InstallDir != ""}
	if s.latest != nil {
		value.Available = newer(s.latest.Version, s.cfg.Version)
	}
	s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.directory(), "status.json"))
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	var activity api.UpdateActivity
	if err := json.Unmarshal(data, &activity); err != nil {
		return value, err
	}
	value.Activity = &activity
	return value, nil
}

func (s *Service) Apply(ctx context.Context, version string) error {
	if s.cfg.InstallDir == "" {
		return ErrNotInstalled
	}
	s.mu.Lock()
	if s.latest == nil || s.latest.Version != version || !newer(version, s.cfg.Version) {
		s.mu.Unlock()
		return ErrConflict
	}
	s.mu.Unlock()
	directory := s.directory()
	if err := os.MkdirAll(directory, 0750); err != nil {
		return err
	}
	requestPath := filepath.Join(directory, "request.json")
	file, err := os.OpenFile(requestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	err = json.NewEncoder(file).Encode(api.ApplySystemUpdate{Version: version})
	err = errors.Join(err, file.Close())
	if err == nil {
		err = writeActivity(directory, version, "queued", nil)
	}
	if err == nil {
		err = s.start(ctx)
	}
	if err != nil {
		removeErr := os.Remove(requestPath)
		return errors.Join(err, removeErr, writeActivity(directory, version, "failed", err))
	}
	return nil
}

func writeActivity(directory, version string, phase api.UpdateActivityPhase, failure error) error {
	value := api.UpdateActivity{Version: version, Phase: phase, UpdatedAt: time.Now().UTC()}
	if failure != nil {
		detail := failure.Error()
		value.Error = &detail
	}
	file, err := os.CreateTemp(directory, ".status-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	err = json.NewEncoder(file).Encode(value)
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(directory, "status.json"))
}
