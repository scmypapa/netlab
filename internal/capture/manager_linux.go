//go:build linux

package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

type Manager struct {
	ovs             *network.OVS
	directory, node string
	mu              sync.Mutex
	live            map[string]*session
}
type session struct {
	mu        sync.Mutex
	detail    api.CaptureDetail
	stats     *aggregate
	cmd       *exec.Cmd
	done      chan struct{}
	stopped   bool
	persisted time.Time
}

func New(ctx context.Context, ovs *network.OVS, directory, node string) (*Manager, error) {
	m := &Manager{ovs: ovs, directory: filepath.Join(directory, "captures"), node: node, live: map[string]*session{}}
	if err := os.MkdirAll(m.directory, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(m.directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := uuid.Parse(entry.Name()); err != nil {
			return nil, fmt.Errorf("无效抓包目录：%s", entry.Name())
		}
		detail, err := m.read(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			if err := m.cleanup(ctx, entry.Name()); err != nil {
				return nil, err
			}
			if err := os.RemoveAll(filepath.Join(m.directory, entry.Name())); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := m.cleanup(ctx, entry.Name()); err != nil {
			return nil, err
		}
		if detail.Segment.Status == api.CaptureSegmentStatusRunning || detail.Segment.Status == api.CaptureSegmentStatusStarting {
			now, message := time.Now().UTC(), "节点进程已重启，抓包结束"
			detail.Segment.Status, detail.Segment.FinishedAt, detail.Segment.Error = api.CaptureSegmentStatusFailed, &now, &message
			if err := m.save(detail); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

func (m *Manager) Start(ctx context.Context, request api.NodeCaptureRequest) (segment api.CaptureSegment, startErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := uuid.Parse(request.Id); err != nil {
		return api.CaptureSegment{}, err
	}
	if request.Settings.DurationSeconds < 10 || request.Settings.DurationSeconds > 1800 || request.Settings.FileSizeMiB < 1 || request.Settings.FileSizeMiB > 1024 {
		return api.CaptureSegment{}, errors.New("抓包时长或文件额度超出范围")
	}
	for _, live := range m.live {
		live.mu.Lock()
		active := live.detail.Segment.EnvironmentId == request.EnvironmentId && (live.detail.Segment.Status == api.CaptureSegmentStatusRunning || live.detail.Segment.Status == api.CaptureSegmentStatusStarting)
		live.mu.Unlock()
		if active {
			return api.CaptureSegment{}, errors.New("该环境在此节点已有运行中的抓包")
		}
	}
	ports, assets := []string{}, []string{}
	for _, iface := range request.Interfaces {
		if iface.NodeId == m.node && slices.Contains(request.Settings.AssetIds, iface.AssetId) {
			ports = append(ports, iface.PortName)
			if !slices.Contains(assets, iface.AssetId) {
				assets = append(assets, iface.AssetId)
			}
		}
	}
	if len(ports) == 0 {
		return api.CaptureSegment{}, errors.New("没有可抓包接口")
	}
	directory := filepath.Join(m.directory, request.Id)
	if err := os.Mkdir(directory, 0700); err != nil {
		return api.CaptureSegment{}, err
	}
	s := &session{done: make(chan struct{}), stats: newAggregate(m.node, request), detail: api.CaptureDetail{Segment: api.CaptureSegment{Id: request.Id, NodeId: m.node, EnvironmentId: request.EnvironmentId, AssetIds: assets, Status: api.CaptureSegmentStatusStarting, StartedAt: time.Now().UTC()}, Flows: []api.CaptureFlow{}}}
	if err := m.save(s.detail); err != nil {
		return api.CaptureSegment{}, errors.Join(err, os.RemoveAll(directory))
	}
	created := false
	defer func() {
		if startErr == nil {
			return
		}
		if created {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			startErr = errors.Join(startErr, m.cleanup(cleanupCtx, request.Id))
			cancel()
		}
		now, message := time.Now().UTC(), startErr.Error()
		s.detail.Segment.Status, s.detail.Segment.Error, s.detail.Segment.FinishedAt = api.CaptureSegmentStatusFailed, &message, &now
		startErr = errors.Join(startErr, m.save(s.detail))
	}()
	device, peer := deviceNames(request.Id)
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: device, Alias: "netlab.capture:" + request.Id}, PeerName: peer}
	err := netlink.LinkAdd(link)
	if err != nil {
		return api.CaptureSegment{}, err
	}
	if err = netlink.LinkSetAlias(link, "netlab.capture:"+request.Id); err != nil {
		return api.CaptureSegment{}, errors.Join(err, netlink.LinkDel(link))
	}
	created = true
	err = netlink.LinkSetMTU(link, 65535)
	if err == nil {
		err = netlink.LinkSetUp(link)
	}
	if err == nil {
		var other netlink.Link
		other, err = netlink.LinkByName(peer)
		if err == nil {
			err = netlink.LinkSetMTU(other, 65535)
		}
		if err == nil {
			err = netlink.LinkSetUp(other)
		}
	}
	if err == nil {
		err = m.ovs.Mirror(ctx, request.Id, request.EnvironmentId, device, ports)
	}
	if err != nil {
		return api.CaptureSegment{}, err
	}
	args := []string{"-l", "-n", "-i", peer, "-w", filepath.Join(directory, "capture.pcapng"), "-P", "-T", "fields", "-E", "occurrence=f", "-a", fmt.Sprint("duration:", request.Settings.DurationSeconds), "-a", fmt.Sprint("filesize:", request.Settings.FileSizeMiB*1024)}
	if request.Settings.Filter != nil && *request.Settings.Filter != "" {
		args = append(args, "-f", *request.Settings.Filter)
	}
	for _, field := range []string{"frame.time_epoch", "frame.len", "eth.src", "eth.dst", "ip.src", "ip.dst", "ipv6.src", "ipv6.dst", "tcp.srcport", "tcp.dstport", "udp.srcport", "udp.dstport", "_ws.col.Protocol"} {
		args = append(args, "-e", field)
	}
	cmd := exec.Command("tshark", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return api.CaptureSegment{}, err
	}
	stderr, err := os.Create(filepath.Join(directory, "stderr.log"))
	if err != nil {
		return api.CaptureSegment{}, err
	}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		stderr.Close()
		return api.CaptureSegment{}, err
	}
	s.cmd = cmd
	m.live[request.Id] = s
	started := s.detail.Segment
	go func() {
		var decodeErr error
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			s.mu.Lock()
			decodeErr = s.stats.add(scanner.Text())
			if decodeErr == nil && (s.detail.Segment.Status == api.CaptureSegmentStatusStarting || time.Since(s.persisted) >= 10*time.Second) {
				s.detail.Segment.Status = api.CaptureSegmentStatusRunning
				decodeErr = m.save(s.snapshot())
				s.persisted = time.Now()
			}
			s.mu.Unlock()
			if decodeErr != nil {
				cmd.Process.Kill()
				break
			}
		}
		waitErr := cmd.Wait()
		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if stopped && isInterrupt(waitErr) {
			waitErr = nil
		}
		finalErr := errors.Join(decodeErr, scanner.Err(), waitErr, stderr.Close())
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		finalErr = errors.Join(finalErr, m.cleanup(cleanupCtx, request.Id))
		cancel()
		s.mu.Lock()
		s.detail = s.snapshot()
		now := time.Now().UTC()
		s.detail.Segment.FinishedAt, s.detail.Segment.Status = &now, api.CaptureSegmentStatusStopped
		if finalErr != nil {
			s.detail.Segment.Status = api.CaptureSegmentStatusFailed
			message := finalErr.Error()
			if detail, readErr := os.ReadFile(filepath.Join(directory, "stderr.log")); readErr == nil {
				message += ": " + string(detail[max(0, len(detail)-8192):])
			}
			s.detail.Segment.Error = &message
		}
		saveErr := m.save(s.detail)
		if saveErr != nil {
			message := saveErr.Error()
			s.detail.Segment.Status, s.detail.Segment.Error = api.CaptureSegmentStatusFailed, &message
		}
		s.mu.Unlock()
		if saveErr == nil {
			m.mu.Lock()
			delete(m.live, request.Id)
			m.mu.Unlock()
		}
		close(s.done)
	}()
	return started, nil
}

func (s *session) snapshot() api.CaptureDetail {
	detail := s.detail
	detail.Segment.Bytes, detail.Segment.Packets, detail.Segment.OmittedFlows = s.stats.bytes, s.stats.packets, s.stats.omitted
	detail.Flows = s.stats.snapshot()
	return detail
}
func (m *Manager) Get(id, environment string) (api.CaptureDetail, error) {
	if _, err := uuid.Parse(id); err != nil {
		return api.CaptureDetail{}, err
	}
	m.mu.Lock()
	session := m.live[id]
	m.mu.Unlock()
	var detail api.CaptureDetail
	var err error
	if session != nil {
		session.mu.Lock()
		detail = session.snapshot()
		session.mu.Unlock()
	} else {
		detail, err = m.read(id)
	}
	if err == nil && detail.Segment.EnvironmentId != environment {
		err = os.ErrNotExist
	}
	return detail, err
}
func (m *Manager) List(environment string) ([]api.CaptureSegment, error) {
	entries, err := os.ReadDir(m.directory)
	if err != nil {
		return nil, err
	}
	segments := []api.CaptureSegment{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		detail, err := m.Get(entry.Name(), environment)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		segments = append(segments, detail.Segment)
	}
	return segments, nil
}
func (m *Manager) Stop(ctx context.Context, id, environment string) (api.CaptureSegment, error) {
	detail, err := m.Get(id, environment)
	if err != nil {
		return api.CaptureSegment{}, err
	}
	m.mu.Lock()
	s := m.live[id]
	m.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		if (s.detail.Segment.Status == api.CaptureSegmentStatusRunning || s.detail.Segment.Status == api.CaptureSegmentStatusStarting) && !s.stopped {
			s.stopped = true
			err = s.cmd.Process.Signal(syscall.SIGINT)
		}
		s.mu.Unlock()
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			return api.CaptureSegment{}, err
		}
		select {
		case <-s.done:
		case <-ctx.Done():
			return api.CaptureSegment{}, ctx.Err()
		}
		detail, err = m.Get(id, environment)
	}
	return detail.Segment, err
}
func (m *Manager) Remove(ctx context.Context, id, environment string) error {
	if _, err := m.Stop(ctx, id, environment); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := m.cleanup(ctx, id); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(m.directory, id)); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.live, id)
	m.mu.Unlock()
	return nil
}
func (m *Manager) RemoveEnvironment(ctx context.Context, environment string) error {
	segments, err := m.List(environment)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		err = errors.Join(err, m.Remove(ctx, segment.Id, environment))
	}
	return err
}
func (m *Manager) StopAssets(ctx context.Context, environment string, assets []api.AssetExecution) error {
	segments, err := m.List(environment)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		if segment.Status != api.CaptureSegmentStatusRunning && segment.Status != api.CaptureSegmentStatusStarting {
			continue
		}
		for _, asset := range assets {
			if slices.Contains(segment.AssetIds, asset.Asset.Id) {
				_, stopErr := m.Stop(ctx, segment.Id, environment)
				err = errors.Join(err, stopErr)
				break
			}
		}
	}
	return err
}
func (m *Manager) Close() {
	m.mu.Lock()
	sessions := map[string]*session{}
	for id, s := range m.live {
		sessions[id] = s
	}
	m.mu.Unlock()
	for id, s := range sessions {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := m.Stop(ctx, id, s.detail.Segment.EnvironmentId)
		cancel()
		if err != nil {
			s.cmd.Process.Kill()
			<-s.done
		}
	}
}
func (m *Manager) File(w http.ResponseWriter, r *http.Request, id, environment string) error {
	detail, err := m.Get(id, environment)
	if err != nil {
		return err
	}
	if detail.Segment.Status == api.CaptureSegmentStatusRunning || detail.Segment.Status == api.CaptureSegmentStatusStarting {
		return errors.New("请先停止抓包再下载")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=capture.pcapng")
	http.ServeFile(w, r, filepath.Join(m.directory, id, "capture.pcapng"))
	return nil
}
func (m *Manager) read(id string) (api.CaptureDetail, error) {
	var detail api.CaptureDetail
	raw, err := os.ReadFile(filepath.Join(m.directory, id, "capture.json"))
	if err == nil {
		err = json.Unmarshal(raw, &detail)
	}
	return detail, err
}
func (m *Manager) save(detail api.CaptureDetail) error {
	directory := filepath.Join(m.directory, detail.Segment.Id)
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(directory, "capture.tmp"), raw, 0600); err != nil {
		return err
	}
	return os.Rename(filepath.Join(directory, "capture.tmp"), filepath.Join(directory, "capture.json"))
}
func deviceNames(id string) (string, string) {
	suffix := strings.ReplaceAll(id, "-", "")[:12]
	return "nm" + suffix, "np" + suffix
}
func (m *Manager) cleanup(ctx context.Context, id string) error {
	if err := m.ovs.RemoveMirror(ctx, id); err != nil {
		return err
	}
	device, _ := deviceNames(id)
	link, err := netlink.LinkByName(device)
	if errors.As(err, new(netlink.LinkNotFoundError)) {
		return nil
	}
	if err != nil {
		return err
	}
	if link.Attrs().Alias != "netlab.capture:"+id {
		return errors.New("抓包端口属于其他对象")
	}
	return netlink.LinkDel(link)
}
func isInterrupt(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGINT
}
