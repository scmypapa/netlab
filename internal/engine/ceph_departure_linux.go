//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"libvirt.org/go/libvirt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"netlab.local/core/api"
)

func (e *Engine) CephAdmin(ctx context.Context, id string) (api.NodeCephAdmin, error) {
	config, err := e.managedCeph(ctx, id, "config", "generate-minimal-conf")
	if err != nil {
		return api.NodeCephAdmin{}, err
	}
	keyring, err := os.ReadFile(filepath.Join(e.cephRoot(id), "ceph.client.admin.keyring"))
	if err != nil {
		return api.NodeCephAdmin{}, err
	}
	key, err := os.ReadFile(filepath.Join(e.cephRoot(id), "ceph.pub"))
	return api.NodeCephAdmin{Config: string(config), Keyring: string(keyring), PublicKey: string(key)}, err
}

func (e *Engine) ImportCephAdmin(id string, input api.NodeCephAdmin) error {
	root := e.cephRoot(id)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	for name, value := range map[string]string{"ceph.conf": input.Config, "ceph.client.admin.keyring": input.Keyring, "ceph.pub": input.PublicKey} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			return err
		}
	}
	return nil
}

// Ceph orchestrator evacuates replicas before removing OSDs. The pool's replica count is unchanged.
func (e *Engine) DepartCeph(ctx context.Context, id string, input api.NodeCephDeparture) error {
	unlock := e.lock("ceph:" + id)
	defer unlock()
	out, err := e.managedCeph(ctx, id, "orch", "host", "ls", "--format", "json")
	if err != nil {
		return err
	}
	var hosts []struct{ Hostname string }
	if err = json.Unmarshal(out, &hosts); err != nil {
		return err
	}
	if !slices.ContainsFunc(hosts, func(h struct{ Hostname string }) bool { return h.Hostname == input.Host }) {
		return nil
	}
	if len(input.Remaining) == 0 {
		return errors.New("保留至少一个 Ceph 存储节点")
	}
	monitors := input.Remaining[:min(3, len(input.Remaining))]
	placement := slices.Clone(monitors)
	if !slices.Contains(placement, input.Host) {
		placement = append(placement, input.Host)
	}
	if _, err = e.managedCeph(ctx, id, "orch", "apply", "mon", "--placement", strings.Join(placement, ";")); err != nil {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		out, err = e.managedCeph(ctx, id, "quorum_status", "--format", "json")
		if err != nil {
			return err
		}
		var quorum struct {
			Names []string `json:"quorum_names"`
		}
		if err = json.Unmarshal(out, &quorum); err != nil {
			return err
		}
		if slices.ContainsFunc(monitors, func(host string) bool { return !slices.Contains(quorum.Names, host) }) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				continue
			}
		}
		break
	}
	// Keep a usable MON seed on the new management host before removing the old MON.
	out, err = e.managedCeph(ctx, id, "config", "generate-minimal-conf")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(e.cephRoot(id), "ceph.conf"), out, 0600); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"orch", "apply", "mon", "--placement", strings.Join(monitors, ";")},
		{"orch", "apply", "mgr", "--placement", strings.Join(monitors, ";")},
		{"orch", "host", "drain", input.Host, "--zap-osd-devices"},
	} {
		if _, err = e.managedCeph(ctx, id, args...); err != nil {
			return err
		}
	}
	for {
		out, err = e.managedCeph(ctx, id, "orch", "ps", "--hostname", input.Host, "--refresh", "--format", "json")
		if err != nil {
			return err
		}
		var daemons []json.RawMessage
		if err = json.Unmarshal(out, &daemons); err != nil {
			return err
		}
		if len(daemons) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	_, err = e.managedCeph(ctx, id, "orch", "host", "rm", input.Host)
	return err
}

// Detach only this node's client registration; shared images belong to the pool.
func (e *Engine) DetachStorage(ctx context.Context, id string) error {
	if e.vm != nil {
		secret, err := e.vm.conn.LookupSecretByUUIDString(id)
		if err == nil {
			defer secret.Free()
			if err = secret.Undefine(); err != nil {
				return err
			}
		} else {
			var native libvirt.Error
			if !errors.As(err, &native) || native.Code != libvirt.ERR_NO_SECRET {
				return err
			}
		}
	}
	return os.RemoveAll(filepath.Join(e.cfg.DataDir, "storage-pools", id))
}
