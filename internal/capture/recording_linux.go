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
	"time"

	"github.com/gopacket/gopacket/pcapgo"
	"netlab.local/core/api"
)

type recordingRun struct {
	cancel     context.CancelFunc
	done       chan struct{}
	interfaces []api.SecurityInterface
}

func (m *Manager) stopRecording(environment string) {
	run := m.recordings[environment]
	run.cancel()
	<-run.done
	delete(m.recordings, environment)
}

func (m *Manager) Reconcile(ctx context.Context, targets []api.SecurityInterface, quotas map[string]int64) (map[string]api.PacketRetention, error) {
	m.control.Lock()
	defer m.control.Unlock()
	attachments, err := m.ovs.CaptureAttachments(ctx)
	if err != nil {
		return nil, err
	}
	actual := map[string][]api.SecurityInterface{}
	for _, target := range targets {
		if target.Interface.NodeId == m.node && quotas[target.EnvironmentId] > 0 && attachments[target.Interface.PortName] != "" {
			actual[target.EnvironmentId] = append(actual[target.EnvironmentId], target)
		}
	}
	all := []api.SecurityInterface{}
	for environment, interfaces := range actual {
		slices.SortFunc(interfaces, func(a, b api.SecurityInterface) int {
			return strings.Compare(a.Interface.PortName, b.Interface.PortName)
		})
		actual[environment] = interfaces
		all = append(all, interfaces...)
	}
	for environment, run := range m.recordings {
		finished := false
		select {
		case <-run.done:
			finished = true
		default:
		}
		if finished || !reflect.DeepEqual(run.interfaces, actual[environment]) {
			m.stopRecording(environment)
		}
	}
	if err = m.recorder.configure(quotas, all); err != nil {
		return nil, err
	}
	for environment, interfaces := range actual {
		if m.recordings[environment] != nil {
			continue
		}
		if err = m.startRecording(environment, interfaces, attachments); err != nil {
			return nil, err
		}
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

func (m *Manager) startRecording(environment string, interfaces []api.SecurityInterface, attachments map[string]string) error {
	ctx, cancel := context.WithCancel(m.ctx)
	args := []string{"-q", "-s", "0", "-B", "2", "-w", "-"}
	for _, target := range interfaces {
		args = append(args, "-i", attachments[target.Interface.PortName], "--ifname", target.Interface.PortName)
	}
	command := exec.CommandContext(ctx, "dumpcap", args...)
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGINT) }
	command.WaitDelay = 5 * time.Second
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		cancel()
		return err
	}
	run := &recordingRun{cancel: cancel, done: make(chan struct{}), interfaces: interfaces}
	m.recordings[environment] = run
	go func() {
		defer close(run.done)
		defer cancel()
		local := make([]api.CaptureInterface, len(interfaces))
		for i, target := range interfaces {
			local[i] = target.Interface
		}
		reader, readErr := pcapgo.NewNgReader(stdout, pcapgo.NgReaderOptions{WantMixedLinkType: true, StatisticsCallback: func(index int, stats pcapgo.NgInterfaceStatistics) {
			if index >= 0 && index < len(local) {
				if err := m.recorder.interfaceStatistics(environment, local[index].PortName, stats); err != nil {
					m.recordingError(environment, err)
				}
				m.recordStatistics(environment, local[index].PortName, stats)
			}
		}})
		if readErr == nil {
			state, release := m.recorder.lockRecording(environment)
			if state != nil {
				state.Error = ""
			}
			release()
			for {
				data, info, packetErr := reader.ZeroCopyReadPacketData()
				if packetErr != nil {
					readErr = packetErr
					break
				}
				if info.InterfaceIndex < 0 || info.InterfaceIndex >= len(local) {
					readErr = errors.New("dumpcap接口索引越界")
					break
				}
				m.recordSubscriptions(environment, local[info.InterfaceIndex].PortName, info, data)
				if err := m.recorder.Record(environment, local, info.InterfaceIndex, info, data); err != nil {
					m.recordingError(environment, err)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		} else if readErr != nil {
			command.Process.Kill()
		}
		waitErr := command.Wait()
		if waitErr != nil && !isInterrupt(waitErr) && !errors.Is(waitErr, context.Canceled) {
			readErr = errors.Join(readErr, waitErr, errors.New(strings.TrimSpace(stderr.String())))
		}
		state, release := m.recorder.lockRecording(environment)
		defer release()
		if state != nil {
			if err := errors.Join(readErr, m.recorder.closePart(environment)); err != nil {
				state.Error = err.Error()
			}
		}
	}()
	return nil
}

func (m *Manager) recordingError(environment string, err error) {
	state, release := m.recorder.lockRecording(environment)
	defer release()
	if state != nil {
		closeErr := m.recorder.closePart(environment)
		state.Error = errors.Join(err, closeErr).Error()
	}
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
