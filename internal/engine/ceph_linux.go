//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

func (e *Engine) cephRoot(id string) string { return filepath.Join(e.cfg.DataDir, "ceph", id) }

func (e *Engine) managedCeph(ctx context.Context, id string, args ...string) ([]byte, error) {
	root := e.cephRoot(id)
	return commandOutput(ctx, "ceph", append([]string{"--conf", filepath.Join(root, "ceph.conf"), "--keyring", filepath.Join(root, "ceph.client.admin.keyring")}, args...)...)
}

func (e *Engine) BootstrapCeph(ctx context.Context, id string) (api.NodeCephBootstrap, error) {
	unlock := e.lock("ceph:" + id)
	defer unlock()
	root := e.cephRoot(id)
	config := filepath.Join(root, "ceph.conf")
	if _, err := os.Stat(config); errors.Is(err, os.ErrNotExist) {
		device, err := e.storageSelection()
		if err != nil {
			return api.NodeCephBootstrap{}, err
		}
		if device == "" {
			return api.NodeCephBootstrap{}, errors.New("节点未指定集群存储盘")
		}
		address, err := network.AccessAddress(e.cfg.AdvertiseAddress)
		if err != nil {
			return api.NodeCephBootstrap{}, err
		}
		if err = os.MkdirAll(root, 0700); err != nil {
			return api.NodeCephBootstrap{}, err
		}
		version, err := commandOutput(ctx, "ceph", "--version")
		if err != nil {
			return api.NodeCephBootstrap{}, err
		}
		fields := strings.Fields(string(version))
		if len(fields) < 3 {
			return api.NodeCephBootstrap{}, errors.New("无法读取 Ceph 客户端版本")
		}
		// Server and host librados use the same release, including key encoding.
		image := "quay.io/ceph/ceph:v" + fields[2]
		if err = command(ctx, "cephadm", "--image", image, "bootstrap", "--fsid", id, "--mon-ip", address,
			"--output-config", config, "--output-keyring", filepath.Join(root, "ceph.client.admin.keyring"),
			"--output-pub-ssh-key", filepath.Join(root, "ceph.pub"), "--skip-dashboard", "--skip-monitoring-stack"); err != nil {
			return api.NodeCephBootstrap{}, err
		}
	} else if err != nil {
		return api.NodeCephBootstrap{}, err
	}
	key, err := os.ReadFile(filepath.Join(root, "ceph.pub"))
	return api.NodeCephBootstrap{PublicKey: strings.TrimSpace(string(key))}, err
}

func (e *Engine) JoinCeph(ctx context.Context, id string, input api.NodeCephBootstrap) (api.NodeCephHost, error) {
	name, err := os.Hostname()
	if err != nil {
		return api.NodeCephHost{}, err
	}
	address, err := network.AccessAddress(e.cfg.AdvertiseAddress)
	if err != nil {
		return api.NodeCephHost{}, err
	}
	host := api.NodeCephHost{Name: name, Address: address}
	device, err := e.storageSelection()
	if err != nil {
		return api.NodeCephHost{}, err
	}
	if device == "" {
		return host, nil
	}
	host.Device = &device
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(input.PublicKey))
	if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
		return api.NodeCephHost{}, errors.New("无效的集群 SSH 公钥")
	}
	unlock := e.lock("ceph-ssh")
	defer unlock()
	if err = os.MkdirAll("/root/.ssh", 0700); err != nil {
		return api.NodeCephHost{}, err
	}
	path := "/root/.ssh/authorized_keys"
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return api.NodeCephHost{}, err
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	lines, found := strings.Split(string(raw), "\n"), false
	for i, line := range lines {
		if strings.Contains(line, public) {
			lines[i], found = public+" netlab-ceph/"+id, true
		}
	}
	if !found {
		lines = append(lines, public+" netlab-ceph/"+id)
	}
	if err = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		return api.NodeCephHost{}, err
	}
	return host, nil
}

func (e *Engine) ConfigureCeph(ctx context.Context, id string, input api.NodeCephConfiguration) (api.CephConnection, error) {
	unlock := e.lock("ceph:" + id)
	defer unlock()
	for _, host := range input.Hosts {
		if host.Device == nil {
			continue
		}
		if _, err := e.managedCeph(ctx, id, "orch", "host", "add", host.Name, host.Address); err != nil {
			return api.CephConnection{}, err
		}
		if host.Device != nil {
			spec := map[string]any{"service_type": "osd", "service_id": "netlab-" + host.Name,
				"placement": map[string]any{"hosts": []string{host.Name}}, "spec": map[string]any{"data_devices": map[string]any{"paths": []string{*host.Device}}}}
			path := filepath.Join(e.cephRoot(id), "osd.json")
			if err := writeRecoveryJSON(path, spec); err != nil {
				return api.CephConnection{}, err
			}
			if _, err := e.managedCeph(ctx, id, "orch", "apply", "-i", path); err != nil {
				return api.CephConnection{}, err
			}
		}
	}
	if _, err := e.managedCeph(ctx, id, "config", "set", "mgr", "mgr/cephadm/autotune_memory_target_ratio", "0.2"); err != nil {
		return api.CephConnection{}, err
	}
	hosts, err := e.managedCeph(ctx, id, "orch", "host", "ls", "--format", "json")
	if err != nil {
		return api.CephConnection{}, err
	}
	var members []struct{ Hostname string }
	if err = json.Unmarshal(hosts, &members); err != nil {
		return api.CephConnection{}, err
	}
	monitors := "1"
	if len(members) >= 3 {
		monitors = "3"
	}
	if _, err = e.managedCeph(ctx, id, "orch", "apply", "mon", "--placement", monitors); err != nil {
		return api.CephConnection{}, err
	}
	replicas := 1
	if input.Replicas != nil {
		replicas = *input.Replicas
	}
	if replicas < 1 || replicas > 3 {
		return api.CephConnection{}, errors.New("副本数应为 1 至 3")
	}
	pool := "netlab"
	out, err := e.managedCeph(ctx, id, "osd", "pool", "ls", "--format", "json")
	if err != nil {
		return api.CephConnection{}, err
	}
	var pools []string
	if err = json.Unmarshal(out, &pools); err != nil {
		return api.CephConnection{}, err
	}
	commands := [][]string{{"osd", "pool", "create", pool}, {"osd", "pool", "application", "enable", pool, "rbd"}}
	if !slices.Contains(pools, pool) || input.Replicas != nil {
		commands = append(commands, []string{"config", "set", "mon", "mon_allow_pool_size_one", "true"},
			[]string{"osd", "pool", "set", pool, "size", fmt.Sprint(replicas), "--yes-i-really-mean-it"},
			[]string{"osd", "pool", "set", pool, "min_size", fmt.Sprint(max(1, replicas-1))})
	}
	for _, args := range commands {
		if _, err := e.managedCeph(ctx, id, args...); err != nil {
			return api.CephConnection{}, err
		}
	}
	if _, err := e.managedCeph(ctx, id, "auth", "get-or-create", "client.netlab", "mon", "profile rbd", "osd", "profile rbd pool="+pool, "mgr", "profile rbd pool="+pool); err != nil {
		return api.CephConnection{}, err
	}
	key, err := e.managedCeph(ctx, id, "auth", "get-key", "client.netlab")
	if err != nil {
		return api.CephConnection{}, err
	}
	monmap, err := e.managedCeph(ctx, id, "mon", "dump", "--format", "json")
	if err != nil {
		return api.CephConnection{}, err
	}
	var monmapData struct {
		Mons []struct {
			PublicAddrs struct {
				Addrvec []struct {
					Type string
					Addr string
				} `json:"addrvec"`
			} `json:"public_addrs"`
		}
	}
	if err = json.Unmarshal(monmap, &monmapData); err != nil {
		return api.CephConnection{}, err
	}
	connection := api.CephConnection{Pool: pool, User: "netlab", Key: ptr(strings.TrimSpace(string(key)))}
	for _, mon := range monmapData.Mons {
		for _, address := range mon.PublicAddrs.Addrvec {
			if address.Type == "v2" {
				endpoint, _, _ := strings.Cut(address.Addr, "/")
				connection.Monitors = append(connection.Monitors, endpoint)
			}
		}
	}
	// A usable pool does not prove that newly joined storage hosts have finished provisioning.
	storageHosts := []string{}
	for _, host := range input.Hosts {
		if host.Device != nil {
			storageHosts = append(storageHosts, host.Name)
		}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		out, err := e.managedCeph(ctx, id, "df", "--format", "json")
		if err != nil {
			return api.CephConnection{}, err
		}
		var df struct {
			Pools []struct {
				Name  string
				Stats struct {
					Available int64 `json:"max_avail"`
				}
			}
		}
		if err = json.Unmarshal(out, &df); err != nil {
			return api.CephConnection{}, err
		}
		for _, p := range df.Pools {
			if p.Name == pool && p.Stats.Available > 0 {
				if len(storageHosts) > 0 {
					out, err := e.managedCeph(ctx, id, "orch", "ps", "--daemon-type", "osd", "--refresh", "--format", "json")
					if err != nil {
						return api.CephConnection{}, err
					}
					var daemons []struct {
						Hostname string
						Status   int
					}
					if err = json.Unmarshal(out, &daemons); err != nil {
						return api.CephConnection{}, err
					}
					ready := true
					for _, host := range storageHosts {
						ready = ready && slices.ContainsFunc(daemons, func(d struct {
							Hostname string
							Status   int
						}) bool {
							return d.Hostname == host && d.Status == 1
						})
					}
					if !ready {
						break
					}
				}
				root := e.cephRoot(id)
				err = command(ctx, "rbd", "--conf", filepath.Join(root, "ceph.conf"), "--keyring", filepath.Join(root, "ceph.client.admin.keyring"), "pool", "init", pool)
				return connection, err
			}
		}
		select {
		case <-ctx.Done():
			return api.CephConnection{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
