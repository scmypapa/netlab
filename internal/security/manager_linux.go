//go:build linux

package security

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

const batchRecords = 5000

type sensorRun struct {
	Devices   []attachment            `json:"devices"`
	Peers     []api.SecurityInterface `json:"peers"`
	Started   time.Time               `json:"started"`
	Rules     time.Time               `json:"rules"`
	directory string
	command   *exec.Cmd
	done      chan error
	output    tailBuffer
}

type Manager struct {
	ovs                            *network.OVS
	directory, node, config, rules string
	mu                             sync.Mutex
	desired                        *sensorRun
	active                         *sensorRun
	sensor                         api.SecuritySensor
	changed                        chan struct{}
	cancel                         context.CancelFunc
	done                           chan struct{}
	poll                           sync.Mutex
	pruned                         time.Time
}

func New(ctx context.Context, ovs *network.OVS, directory, node, config string) (*Manager, error) {
	directory = filepath.Join(directory, "security")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	if config == "" {
		config = "/etc/suricata/suricata.yaml"
	}
	m := &Manager{ovs: ovs, directory: directory, node: node, config: config, rules: "/var/lib/suricata/rules/suricata.rules", changed: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}),
		sensor: api.SecuritySensor{NodeId: node, Status: "idle"}}
	go m.run(ctx)
	return m, nil
}

func (m *Manager) Close() { m.cancel(); <-m.done }

func (m *Manager) Poll(ctx context.Context, request api.NodeSecurityRequest) (api.NodeSecurityObservation, error) {
	m.poll.Lock()
	defer m.poll.Unlock()
	ports, err := m.ovs.CaptureAttachments(ctx)
	if err != nil {
		return api.NodeSecurityObservation{}, err
	}
	desired := &sensorRun{Peers: request.Interfaces, Devices: []attachment{}}
	missing := 0
	for _, target := range request.Interfaces {
		if target.Interface.NodeId == m.node {
			if device := ports[target.Interface.PortName]; device != "" {
				desired.Devices = append(desired.Devices, attachment{Device: device, SecurityInterface: target})
			} else {
				missing++
			}
		}
	}
	slices.SortFunc(desired.Devices, func(a, b attachment) int { return strings.Compare(a.Device, b.Device) })
	slices.SortFunc(desired.Peers, func(a, b api.SecurityInterface) int {
		return strings.Compare(a.Interface.PortName, b.Interface.PortName)
	})
	if info, err := os.Stat(m.rules); err == nil {
		desired.Rules = info.ModTime().UTC()
	}
	m.mu.Lock()
	if m.desired == nil || !reflect.DeepEqual(m.desired.Devices, desired.Devices) || !reflect.DeepEqual(m.desired.Peers, desired.Peers) || !m.desired.Rules.Equal(desired.Rules) {
		m.desired = desired
		m.sensor.Status, m.sensor.Error = "loading", nil
		select {
		case m.changed <- struct{}{}:
		default:
		}
	}
	observation := api.NodeSecurityObservation{Cursor: request.Cursor, Alerts: []api.SecurityAlert{}, Sensor: m.sensor, Coverage: map[string]int{}}
	if m.active != nil {
		for _, device := range m.active.Devices {
			observation.Coverage[device.EnvironmentId]++
		}
	}
	active := m.active
	if active != m.desired {
		active = nil
	}
	m.mu.Unlock()
	observation.Sensor.CheckedAt = time.Now().UTC()
	if err = m.read(ctx, request.Cursor, active, &observation); err != nil {
		return observation, err
	}
	if active != nil {
		m.mu.Lock()
		if m.active == active && observation.Sensor.Status == "ready" {
			m.sensor.Status, m.sensor.Packets, m.sensor.DroppedPackets = "ready", observation.Sensor.Packets, observation.Sensor.DroppedPackets
		}
		m.mu.Unlock()
	}
	if missing > 0 && observation.Sensor.Status != "failed" && observation.Sensor.Status != "unavailable" {
		message := fmt.Sprintf("%d 个资产接口尚未接入检测", missing)
		observation.Sensor.Status, observation.Sensor.Error = "loading", &message
	}
	if time.Since(m.pruned) >= time.Hour {
		if err = m.prune(active); err != nil {
			return observation, err
		}
		m.pruned = time.Now()
	}
	return observation, nil
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	var active *sensorRun
	defer func() {
		if active != nil {
			stopSensor(active)
		}
	}()
	for {
		var exited <-chan error
		if active != nil {
			exited = active.done
		}
		select {
		case <-ctx.Done():
			return
		case err := <-exited:
			message := fmt.Sprintf("Suricata 已退出：%v %s", err, active.output.String())
			m.mu.Lock()
			m.sensor.Status, m.sensor.Error = "failed", &message
			m.active = nil
			m.mu.Unlock()
			active = nil
		case <-m.changed:
			if active != nil {
				stopSensor(active)
				active = nil
			}
			m.mu.Lock()
			desired := m.desired
			m.active = nil
			m.mu.Unlock()
			if len(desired.Devices) == 0 {
				m.mu.Lock()
				m.sensor = api.SecuritySensor{NodeId: m.node, Status: "idle"}
				m.mu.Unlock()
				continue
			}
			err := m.start(ctx, desired)
			m.mu.Lock()
			m.sensor = api.SecuritySensor{NodeId: m.node, Status: "loading", Interfaces: len(desired.Devices), StartedAt: &desired.Started}
			if !desired.Rules.IsZero() {
				m.sensor.RulesUpdatedAt = &desired.Rules
			}
			if err != nil {
				message := err.Error()
				m.sensor.Status, m.sensor.Error = "failed", &message
				if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
					m.sensor.Status = "unavailable"
				}
			} else {
				active, m.active = desired, desired
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) start(ctx context.Context, run *sensorRun) error {
	program, err := exec.LookPath("suricata")
	if err != nil {
		return fmt.Errorf("节点未安装 Suricata：%w", err)
	}
	raw, err := os.ReadFile(m.config)
	if err != nil {
		return err
	}
	var config map[string]any
	if err = yaml.Unmarshal(raw, &config); err != nil {
		return err
	}
	if run.Rules.IsZero() {
		return fmt.Errorf("Suricata 规则未准备：运行 suricata-update 下载 ET Open 规则")
	}
	run.Started = time.Now().UTC()
	run.directory = filepath.Join(m.directory, run.Started.Format("20060102T150405.000000000"))
	staging := run.directory + ".tmp"
	if err = os.Mkdir(staging, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	metadata, err := json.Marshal(run)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(staging, "interfaces.json"), metadata, 0600); err != nil {
		return err
	}
	devices := make([]any, 0, len(run.Devices))
	for index, device := range run.Devices {
		devices = append(devices, map[string]any{"interface": device.Device, "threads": 1, "cluster-id": index + 1, "cluster-type": "cluster_flow", "defrag": true,
			"use-mmap": true, "tpacket-v3": true, "ring-size": 1024, "block-size": 65536, "checksum-checks": "no"})
	}
	config["af-packet"], config["runmode"] = devices, "workers"
	config["vlan"], config["livedev"] = map[string]any{"use-for-tracking": true}, map[string]any{"use-for-tracking": true}
	vars, ok := config["vars"].(map[string]any)
	if !ok {
		return fmt.Errorf("Suricata 配置缺少 vars")
	}
	addresses, ok := vars["address-groups"].(map[string]any)
	if !ok {
		return fmt.Errorf("Suricata 配置缺少地址变量")
	}
	addresses["HOME_NET"], addresses["EXTERNAL_NET"] = "any", "any"
	config["rule-files"] = []string{m.rules}
	config["outputs"] = []any{map[string]any{"eve-log": map[string]any{"enabled": true, "filetype": "regular", "filename": "eve-%Y%m%d%H.json", "rotate-interval": "1h", "ethernet": true,
		"types": []any{map[string]any{"alert": map[string]any{"metadata": false, "payload": false, "packet": false}}, map[string]any{"stats": map[string]any{"totals": true, "threads": false}}}}}}
	config["stats"] = map[string]any{"enabled": true, "interval": 10}
	config["logging"] = map[string]any{"default-log-level": "notice", "outputs": []any{map[string]any{"console": map[string]any{"enabled": true}}}}
	config["unix-command"] = map[string]any{"enabled": false}
	config["pid-file"] = filepath.Join(run.directory, "suricata.pid")
	config["default-log-dir"] = run.directory
	compiled, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	path := filepath.Join(run.directory, "suricata.yaml")
	if err = os.WriteFile(filepath.Join(staging, "suricata.yaml"), append([]byte("%YAML 1.1\n---\n"), compiled...), 0600); err != nil {
		return err
	}
	if err = os.Rename(staging, run.directory); err != nil {
		return err
	}
	run.command = exec.Command(program, "-c", path, "--af-packet", "-k", "none", "--init-errors-fatal")
	run.command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	run.command.Stdout, run.command.Stderr = &run.output, &run.output
	if err = run.command.Start(); err != nil {
		return err
	}
	run.done = make(chan error, 1)
	go func() { run.done <- run.command.Wait() }()
	return nil
}

func stopSensor(run *sensorRun) {
	if err := run.command.Process.Signal(syscall.SIGINT); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Error("Suricata stop", "error", err)
	}
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		run.command.Process.Kill()
		<-run.done
	}
}

func (m *Manager) read(ctx context.Context, cursor string, active *sensorRun, result *api.NodeSecurityObservation) error {
	runID, fileID, offset := "", "", int64(0)
	if cursor != "" {
		name, raw, found := strings.Cut(cursor, ":")
		var err error
		offset, err = strconv.ParseInt(raw, 10, 64)
		runID, fileID, found = strings.Cut(name, "/")
		if !found || err != nil || offset < 0 || strings.ContainsAny(runID+fileID, "/\\") || !filepath.IsLocal(name) {
			return fmt.Errorf("无效安全观测游标")
		}
	}
	entries, err := os.ReadDir(m.directory)
	if err != nil {
		return err
	}
	records, foundCursor := 0, runID == ""
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if _, err := time.Parse("20060102T150405.000000000", entry.Name()); err != nil {
			return fmt.Errorf("无效安全观测目录：%s", entry.Name())
		}
		if entry.Name() < runID {
			continue
		}
		directory := filepath.Join(m.directory, entry.Name())
		raw, err := os.ReadFile(filepath.Join(directory, "interfaces.json"))
		if err != nil {
			return err
		}
		var run sensorRun
		if err = json.Unmarshal(raw, &run); err != nil {
			return err
		}
		decoder := newEVEDecoder(m.node, run.Devices, run.Peers)
		files, err := filepath.Glob(filepath.Join(directory, "eve-*.json"))
		if err != nil {
			return err
		}
		for _, filename := range files {
			name := filepath.Base(filename)
			if entry.Name() == runID && name < fileID {
				continue
			}
			if entry.Name() == runID && name == fileID {
				foundCursor = true
			} else {
				offset = 0
			}
			file, err := os.Open(filename)
			if err != nil {
				return err
			}
			if _, err = file.Seek(offset, io.SeekStart); err != nil {
				file.Close()
				return err
			}
			reader := bufio.NewReaderSize(file, 64<<10)
			for records < batchRecords {
				if err = ctx.Err(); err != nil {
					file.Close()
					return err
				}
				line, readErr := reader.ReadBytes('\n')
				if readErr == io.EOF {
					break
				} // An incomplete final line is consumed only after Suricata finishes it.
				if readErr != nil {
					file.Close()
					return readErr
				}
				event, alert, err := decoder.decode(line)
				if err != nil {
					file.Close()
					return fmt.Errorf("EVE %s:%d: %w", entry.Name(), offset, err)
				}
				offset += int64(len(line))
				result.Cursor = fmt.Sprintf("%s/%s:%d", entry.Name(), name, offset)
				records++
				if alert != nil {
					result.Alerts = append(result.Alerts, *alert)
				}
				if event.Type == "stats" && active != nil && active.directory == directory {
					result.Sensor.Packets, result.Sensor.DroppedPackets = event.Stats.Capture.Packets, event.Stats.Capture.Drops
					if result.Sensor.Status == "loading" {
						result.Sensor.Status = "ready"
					}
				}
			}
			if err = file.Close(); err != nil {
				return err
			}
			if records == batchRecords {
				result.HasMore = true
				break
			}
		}
		if records == batchRecords {
			break
		}
	}
	result.HistoryGap = !foundCursor
	return nil
}

func (m *Manager) prune(active *sensorRun) error {
	entries, err := os.ReadDir(m.directory)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-48 * time.Hour)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		directory := filepath.Join(m.directory, entry.Name())
		files, err := filepath.Glob(filepath.Join(directory, "eve-*.json"))
		if err != nil {
			return err
		}
		remaining := 0
		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil {
				return err
			}
			if info.ModTime().Before(cutoff) {
				if err = os.Remove(file); err != nil {
					return err
				}
			} else {
				remaining++
			}
		}
		started, err := time.Parse("20060102T150405.000000000", entry.Name())
		if err != nil {
			return err
		}
		if remaining == 0 && started.Before(cutoff) && (active == nil || active.directory != directory) {
			if err = os.RemoveAll(directory); err != nil {
				return err
			}
		}
	}
	return nil
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = bytes.Clone(b.data[len(b.data)-8192:])
	}
	return len(p), nil
}
func (b *tailBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.data) }
