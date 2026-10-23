//go:build linux

package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
	"github.com/gopacket/gopacket/pcapgo"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

type Manager struct {
	ovs             *network.OVS
	directory, node string
	mu              sync.Mutex
	live            map[string]*session
	control         sync.Mutex
	recorder        *Recorder
	recording       *recordingRun
	flushCancel     context.CancelFunc
}
type session struct {
	mu        sync.Mutex
	detail    api.CaptureDetail
	stats     *aggregate
	writer    *packetWriter
	filter    *pcap.BPF
	cancel    context.CancelFunc
	done      chan struct{}
	stopped   bool
	persisted time.Time
	limit     int64
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
		if _, err = uuid.Parse(entry.Name()); err != nil {
			return nil, err
		}
		detail, readErr := m.read(entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		if detail.Segment.Status == api.CaptureSegmentStatusRunning || detail.Segment.Status == api.CaptureSegmentStatusStarting {
			now, message := time.Now().UTC(), "节点进程已重启，抓包结束"
			detail.Segment.Status, detail.Segment.FinishedAt, detail.Segment.Error = api.CaptureSegmentStatusFailed, &now, &message
			if err = m.save(detail); err != nil {
				return nil, err
			}
		}
	}
	m.recorder, err = NewRecorder(directory, node)
	if err != nil {
		return nil, err
	}
	flushContext, cancel := context.WithCancel(ctx)
	m.flushCancel = cancel
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-flushContext.Done():
				return
			case <-ticker.C:
				m.recorder.Flush()
			}
		}
	}()
	return m, nil
}
func (m *Manager) Start(ctx context.Context, request api.NodeCaptureRequest) (api.CaptureSegment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := uuid.Parse(request.Id); err != nil {
		return api.CaptureSegment{}, err
	}
	settings := request.Settings
	if settings.DurationSeconds < 10 || settings.DurationSeconds > 1800 || settings.FileSizeMiB < 1 || settings.FileSizeMiB > 1024 {
		return api.CaptureSegment{}, errors.New("抓包时长或文件额度超出范围")
	}
	for _, active := range m.live {
		if active.detail.Segment.EnvironmentId == request.EnvironmentId {
			return api.CaptureSegment{}, errors.New("该环境在此节点已有运行中的抓包")
		}
	}
	local := []api.CaptureInterface{}
	assets := []string{}
	for _, iface := range request.Interfaces {
		if iface.NodeId == m.node && slices.Contains(settings.AssetIds, iface.AssetId) {
			local = append(local, iface)
			if !slices.Contains(assets, iface.AssetId) {
				assets = append(assets, iface.AssetId)
			}
		}
	}
	if len(local) == 0 {
		return api.CaptureSegment{}, errors.New("没有可抓包的运行接口")
	}
	var filter *pcap.BPF
	var err error
	if settings.Filter != nil && *settings.Filter != "" {
		filter, err = pcap.NewBPF(layers.LinkTypeEthernet, 262144, *settings.Filter)
		if err != nil {
			return api.CaptureSegment{}, fmt.Errorf("抓包过滤器：%w", err)
		}
	}
	now := time.Now().UTC()
	legacyDirectory := filepath.Join(m.directory, request.Id)
	if err = os.Mkdir(legacyDirectory, 0700); err != nil {
		return api.CaptureSegment{}, err
	}
	m.recorder.mu.Lock()
	writer, err := m.recorder.startPart(request.EnvironmentId, local, request.Id, now)
	m.recorder.mu.Unlock()
	if err != nil {
		return api.CaptureSegment{}, errors.Join(err, os.RemoveAll(legacyDirectory))
	}
	s := &session{done: make(chan struct{}), writer: writer, filter: filter, limit: int64(settings.FileSizeMiB) << 20, stats: newAggregate(m.node, request), detail: api.CaptureDetail{Segment: api.CaptureSegment{Id: request.Id, NodeId: m.node, EnvironmentId: request.EnvironmentId, AssetIds: assets, Status: api.CaptureSegmentStatusStarting, StartedAt: now}, Flows: []api.CaptureFlow{}}}
	for _, iface := range local {
		s.stats.capturePorts = append(s.stats.capturePorts, iface.PortName)
	}
	if err = m.save(s.detail); err != nil {
		m.recorder.mu.Lock()
		cleanupErr := m.recorder.finishWriter(request.EnvironmentId, writer)
		m.recorder.mu.Unlock()
		return api.CaptureSegment{}, errors.Join(err, cleanupErr, os.RemoveAll(legacyDirectory))
	}
	lifetime, cancel := context.WithTimeout(context.Background(), time.Duration(settings.DurationSeconds)*time.Second)
	s.cancel = cancel
	m.live[request.Id] = s
	go func() {
		<-lifetime.Done()
		s.mu.Lock()
		s.stopped = true
		finishErr := func() error {
			m.recorder.mu.Lock()
			defer m.recorder.mu.Unlock()
			return m.recorder.finishWriter(request.EnvironmentId, writer)
		}()
		s.detail = s.snapshot()
		now := time.Now().UTC()
		s.detail.Segment.FinishedAt = &now
		if s.detail.Segment.Error == nil && finishErr == nil {
			s.detail.Segment.Status = api.CaptureSegmentStatusStopped
		} else {
			s.detail.Segment.Status = api.CaptureSegmentStatusFailed
			if finishErr != nil {
				message := finishErr.Error()
				s.detail.Segment.Error = &message
			}
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
	return s.detail.Segment, nil
}
func (m *Manager) recordSubscriptions(environment, portName string, info gopacket.CaptureInfo, data []byte) {
	m.mu.Lock()
	sessions := []*session{}
	for _, s := range m.live {
		if s.detail.Segment.EnvironmentId == environment {
			sessions = append(sessions, s)
		}
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.mu.Lock()
		index := slices.Index(s.stats.capturePorts, portName)
		if index < 0 || s.stopped || info.Timestamp.Before(s.detail.Segment.StartedAt) || s.filter != nil && !s.filter.Matches(info, data) {
			s.mu.Unlock()
			continue
		}
		if s.writer.part.Raw+int64(len(data))+32 > s.limit {
			s.stopped = true
			s.cancel()
			s.mu.Unlock()
			continue
		}
		info.InterfaceIndex = index
		m.recorder.mu.Lock()
		err := s.writer.packets.WritePacket(info, data)
		if err == nil {
			s.writer.part.Raw += int64(len(data)) + 32
			s.writer.part.Packets++
			s.writer.part.End = info.Timestamp.UTC()
		}
		m.recorder.mu.Unlock()
		if err != nil {
			message := err.Error()
			s.detail.Segment.Error = &message
			s.stopped = true
			s.cancel()
			s.mu.Unlock()
			continue
		}
		packet := summarizePacket(0, data, info, layers.LinkTypeEthernet)
		sample := Sample{At: info.Timestamp, Source: packet.Source, Destination: packet.Destination, Protocol: packet.Protocol, SourcePort: packet.SourcePort, DestinationPort: packet.DestinationPort, PortName: portName, Bytes: int64(len(data)), Packets: 1, Samples: 1, CaptureIndex: &index}
		if packet.SourceMac != nil {
			sample.SourceMAC = *packet.SourceMac
		}
		if packet.DestinationMac != nil {
			sample.DestinationMAC = *packet.DestinationMac
		}
		s.stats.addSample(sample)
		s.detail.Segment.Status = api.CaptureSegmentStatusRunning
		if time.Since(s.persisted) >= 10*time.Second {
			if err = m.save(s.snapshot()); err != nil {
				message := err.Error()
				s.detail.Segment.Error = &message
				s.stopped = true
				s.cancel()
			}
			s.persisted = time.Now()
		}
		s.mu.Unlock()
	}
}
func (m *Manager) recordStatistics(environment, portName string, stats pcapgo.NgInterfaceStatistics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.live {
		if s.detail.Segment.EnvironmentId != environment {
			continue
		}
		s.mu.Lock()
		index := slices.Index(s.stats.capturePorts, portName)
		if index < 0 || s.stopped {
			s.mu.Unlock()
			continue
		}
		m.recorder.mu.Lock()
		err := s.writer.packets.WriteInterfaceStats(index, stats)
		m.recorder.mu.Unlock()
		// dumpcap emits one cumulative ISB per interface when a physical capture closes.
		if stats.PacketsDropped != pcapgo.NgNoValue64 {
			if s.detail.Segment.KernelDroppedPackets == nil {
				s.detail.Segment.KernelDroppedPackets = new(int64)
			}
			*s.detail.Segment.KernelDroppedPackets += int64(stats.PacketsDropped)
		}
		if err != nil {
			message := err.Error()
			s.detail.Segment.Error = &message
			s.stopped = true
			s.cancel()
		}
		s.mu.Unlock()
	}
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
	s := m.live[id]
	m.mu.Unlock()
	var detail api.CaptureDetail
	var err error
	if s != nil {
		s.mu.Lock()
		detail = s.snapshot()
		s.mu.Unlock()
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
	result := []api.CaptureSegment{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		detail, readErr := m.Get(entry.Name(), environment)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, detail.Segment)
	}
	return result, nil
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
		s.stopped = true
		s.cancel()
		s.mu.Unlock()
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
	if m.recorder != nil {
		m.recorder.mu.Lock()
		if state := m.recorder.environments[environment]; state != nil {
			for index := len(state.Parts) - 1; index >= 0; index-- {
				part := state.Parts[index]
				if part.CaptureID == id {
					if err := os.Remove(m.recorder.filename(environment, part.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
						m.recorder.mu.Unlock()
						return err
					}
					state.Parts = slices.Delete(state.Parts, index, index+1)
				}
			}
			if err := m.recorder.save(environment); err != nil {
				m.recorder.mu.Unlock()
				return err
			}
		}
		m.recorder.mu.Unlock()
	}
	return os.RemoveAll(filepath.Join(m.directory, id))
}
func (m *Manager) RemoveEnvironment(ctx context.Context, environment string) error {
	segments, err := m.List(environment)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		err = errors.Join(err, m.Remove(ctx, segment.Id, environment))
	}
	if err == nil && m.recorder != nil {
		err = m.recorder.Remove(environment)
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
	if m.flushCancel != nil {
		m.flushCancel()
	}
	m.control.Lock()
	if m.recording != nil {
		m.stopRecording()
	}
	m.control.Unlock()
	m.mu.Lock()
	sessions := map[string]*session{}
	for id, s := range m.live {
		sessions[id] = s
	}
	m.mu.Unlock()
	for id, s := range sessions {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		m.Stop(ctx, id, s.detail.Segment.EnvironmentId)
		cancel()
	}
	if m.recorder != nil {
		m.recorder.Close()
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
	if m.recorder != nil {
		parts, openErr := m.recorder.openParts(environment)
		if openErr != nil {
			return openErr
		}
		defer closeParts(parts)
		for _, part := range parts {
			if part.part.CaptureID == id {
				if err = m.recorder.openPart(environment, &part); err != nil {
					return err
				}
				defer closeParts([]partRead{part})
				input := completePartReader(r.Context(), part, nil)
				defer input.Close()
				_, err = io.Copy(w, input)
				return err
			}
		}
	}
	http.ServeFile(w, r, filepath.Join(m.directory, id, "capture.pcapng"))
	return nil
}
func (m *Manager) read(id string) (api.CaptureDetail, error) {
	var detail api.CaptureDetail
	data, err := os.ReadFile(filepath.Join(m.directory, id, "capture.json"))
	if err == nil {
		err = json.Unmarshal(data, &detail)
	}
	return detail, err
}
func (m *Manager) save(detail api.CaptureDetail) error {
	data, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	directory := filepath.Join(m.directory, detail.Segment.Id)
	if err = os.WriteFile(filepath.Join(directory, "capture.tmp"), data, 0600); err != nil {
		return err
	}
	return os.Rename(filepath.Join(directory, "capture.tmp"), filepath.Join(directory, "capture.json"))
}
func isInterrupt(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGINT
}
