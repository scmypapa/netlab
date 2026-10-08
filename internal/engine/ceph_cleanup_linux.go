//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (e *Engine) PrepareCephRemoval(ctx context.Context, id string) error {
	if _, err := os.Stat(filepath.Join(e.cephRoot(id), "ceph.conf")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	out, err := e.managedCeph(ctx, id, "osd", "pool", "ls", "--format", "json")
	if err != nil {
		return err
	}
	var pools []string
	if err = json.Unmarshal(out, &pools); err != nil {
		return err
	}
	for _, pool := range pools {
		if pool != "netlab" && pool != ".mgr" {
			return errors.New("集群仍包含其他存储池")
		}
	}
	root := e.cephRoot(id)
	out, err = commandOutput(ctx, "rbd", "--conf", filepath.Join(root, "ceph.conf"), "--keyring", filepath.Join(root, "ceph.client.admin.keyring"), "ls", "netlab", "--format", "json")
	if err != nil {
		return err
	}
	var images []string
	if err = json.Unmarshal(out, &images); err != nil {
		return err
	}
	if len(images) > 0 {
		return errors.New("集群仍包含虚拟磁盘")
	}
	_, err = e.managedCeph(ctx, id, "orch", "pause")
	return err
}

func (e *Engine) RemoveCeph(ctx context.Context, id string) error {
	if e.cfg.StorageDevice == "" {
		return nil
	}
	unlock := e.lock("ceph:" + id)
	defer unlock()
	out, err := commandOutput(ctx, "cephadm", "ls")
	if err != nil {
		return err
	}
	var daemons []struct {
		Fsid string `json:"fsid"`
	}
	if err = json.Unmarshal(out, &daemons); err != nil {
		return err
	}
	if slices.ContainsFunc(daemons, func(d struct {
		Fsid string `json:"fsid"`
	}) bool { return d.Fsid == id }) {
		if err = command(ctx, "cephadm", "rm-cluster", "--fsid", id, "--force", "--zap-osds"); err != nil {
			return err
		}
	}
	path := "/root/.ssh/authorized_keys"
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		lines := strings.Split(string(raw), "\n")
		lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasSuffix(line, " netlab-ceph/"+id) })
		if err = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
			return err
		}
	}
	return os.RemoveAll(e.cephRoot(id))
}
