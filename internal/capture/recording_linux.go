//go:build linux

package capture

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"syscall"

	"github.com/gopacket/gopacket/pcapgo"
	"netlab.local/core/api"
)

type recordingRun struct {
	command    *exec.Cmd
	done       chan struct{}
	interfaces []api.SecurityInterface
}

func (m *Manager) stopRecording() {
	run := m.recording
	select {
	case <-run.done:
	default:
		run.command.Process.Signal(syscall.SIGINT)
		<-run.done
	}
	m.recording = nil
}
func (m *Manager) Reconcile(ctx context.Context, targets []api.SecurityInterface, quotas map[string]int64) (map[string]api.PacketRetention, error) {
	m.control.Lock()
	defer m.control.Unlock()
	attachments, err := m.ovs.CaptureAttachments(ctx)
	if err != nil {
		return nil, err
	}
	actual := []api.SecurityInterface{}
	for _, target := range targets {
		if target.Interface.NodeId == m.node && attachments[target.Interface.PortName] != "" {
			actual = append(actual, target)
		}
	}
	slices.SortFunc(actual, func(a, b api.SecurityInterface) int {
		return strings.Compare(a.Interface.PortName, b.Interface.PortName)
	})
	changed := m.recording == nil || !reflect.DeepEqual(m.recording.interfaces, actual)
	if m.recording != nil {
		select {
		case <-m.recording.done:
			changed = true
		default:
		}
	}
	if changed && m.recording != nil {
		m.stopRecording()
	}
	if err = m.recorder.configure(quotas, actual); err != nil {
		return nil, err
	}
	if changed && len(actual) > 0 {
		args := []string{"-q", "-s", "0", "-B", "2", "-w", "-"}
		for _, target := range actual {
			args = append(args, "-i", attachments[target.Interface.PortName], "--ifname", target.Interface.PortName)
		}
		command := exec.Command("dumpcap", args...)
		command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
		stdout, pipeErr := command.StdoutPipe()
		if pipeErr != nil {
			return nil, pipeErr
		}
		var stderr strings.Builder
		command.Stderr = &stderr
		if err = command.Start(); err != nil {
			return nil, err
		}
		m.recorder.mu.Lock()
		m.recorder.err = nil
		m.recorder.mu.Unlock()
		run := &recordingRun{command: command, done: make(chan struct{}), interfaces: actual}
		m.recording = run
		go func() {
			defer close(run.done)
			reader, readErr := pcapgo.NewNgReader(stdout, pcapgo.NgReaderOptions{WantMixedLinkType: true, StatisticsCallback: func(index int, stats pcapgo.NgInterfaceStatistics) {
				if index >= 0 && index < len(actual) {
					target := actual[index]
					if err := m.recorder.interfaceStatistics(target.EnvironmentId, target.Interface.PortName, stats); err != nil {
						m.recorder.mu.Lock()
						m.recorder.err = err
						m.recorder.mu.Unlock()
					}
					m.recordStatistics(target.EnvironmentId, target.Interface.PortName, stats)
				}
			}})
			local := map[string][]api.CaptureInterface{}
			indices := map[string]map[string]int{}
			for _, iface := range actual {
				indices[iface.EnvironmentId] = map[string]int{}
			}
			for _, target := range actual {
				indices[target.EnvironmentId][target.Interface.PortName] = len(local[target.EnvironmentId])
				local[target.EnvironmentId] = append(local[target.EnvironmentId], target.Interface)
			}
			if readErr == nil {
				for {
					data, info, packetErr := reader.ZeroCopyReadPacketData()
					if packetErr != nil {
						readErr = packetErr
						break
					}
					if info.InterfaceIndex >= len(actual) {
						readErr = errors.New("dumpcap接口索引越界")
						break
					}
					target := actual[info.InterfaceIndex]
					m.recordSubscriptions(target.EnvironmentId, target.Interface.PortName, info, data)
					if recordErr := m.recorder.Record(target.EnvironmentId, local[target.EnvironmentId], indices[target.EnvironmentId][target.Interface.PortName], info, data); recordErr != nil {
						m.recorder.mu.Lock()
						if state := m.recorder.environments[target.EnvironmentId]; state != nil {
							state.Error = recordErr.Error()
							m.recorder.closePart(target.EnvironmentId)
						}
						m.recorder.mu.Unlock()
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				readErr = nil
			} else if readErr != nil {
				command.Process.Kill()
			}
			waitErr := command.Wait()
			if waitErr != nil && !isInterrupt(waitErr) {
				readErr = errors.Join(readErr, waitErr, errors.New(strings.TrimSpace(stderr.String())))
			}
			closeErr := m.recorder.Close()
			m.recorder.mu.Lock()
			m.recorder.err = errors.Join(readErr, closeErr)
			m.recorder.mu.Unlock()
		}()
	}
	retention := m.recorder.Retention()
	result := map[string]api.PacketRetention{}
	for environment := range quotas {
		if info, exists := retention[environment]; exists {
			result[environment] = info
		}
	}
	return result, nil
}
func (m *Manager) Recent(ctx context.Context, environment string, q RecentQuery) (api.RecentPacketPage, error) {
	return m.recorder.Recent(ctx, environment, q)
}
func (m *Manager) Retention() map[string]api.PacketRetention { return m.recorder.Retention() }
func (m *Manager) RecentPacket(ctx context.Context, environment, id string) (api.CapturePacketDetail, error) {
	return m.recorder.RecentPacket(ctx, environment, id)
}
func (m *Manager) ExportRecent(ctx context.Context, environment string, q RecentQuery, output io.Writer) error {
	return m.recorder.Export(ctx, environment, q, output)
}
