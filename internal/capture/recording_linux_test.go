//go:build linux

package capture

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"netlab.local/core/api"
)

func BenchmarkPacketDispatchAcrossEnvironments(b *testing.B) {
	for _, count := range []int{0, 64, 1024} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			m := &Manager{live: map[string]*session{}}
			for i := range count {
				m.live[strconv.Itoa(i)] = &session{}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				m.recordSubscriptions("without-subscription", "tap", gopacket.CaptureInfo{}, nil)
			}
		})
	}
}

func TestDumpcapRecordingIsolatedByEnvironment(t *testing.T) {
	if os.Getenv("NETLAB_TEST_DUMPCAP") != "1" {
		t.Skip("NETLAB_TEST_DUMPCAP=1 enables a real loopback capture")
	}
	if _, err := exec.LookPath("dumpcap"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, one, two := uuid.NewString(), uuid.NewString(), uuid.NewString()
	r, err := NewRecorder(t.TempDir(), node)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	m := &Manager{ctx: ctx, directory: t.TempDir(), node: node, recorder: r, recordings: map[string]*recordingRun{}, live: map[string]*session{}}
	defer m.Close()
	if err = r.configure(map[string]int64{one: 1 << 20, two: 1 << 20}, nil); err != nil {
		t.Fatal(err)
	}
	requests := map[string]api.NodeCaptureRequest{}
	for _, environment := range []string{one, two} {
		iface := api.CaptureInterface{NodeId: node, AssetId: "guest", AssetName: "Guest", InterfaceId: "nic", PortName: environment, NetworkId: "lan", Address: "127.0.0.1"}
		if err = m.startRecording(environment, []api.SecurityInterface{{EnvironmentId: environment, Interface: iface}}, map[string]string{environment: "lo"}); err != nil {
			t.Fatal(err)
		}
		request := api.NodeCaptureRequest{Id: uuid.NewString(), EnvironmentId: environment, Interfaces: []api.CaptureInterface{iface}, Settings: api.CreateCapture{AssetIds: []string{"guest"}, DurationSeconds: 10, FileSizeMiB: 1}}
		if _, err = m.Start(ctx, request); err != nil {
			t.Fatal(err)
		}
		requests[environment] = request
		if _, err = m.Start(ctx, request); err == nil {
			t.Fatal("concurrent capture in the same environment was accepted")
		}
	}
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	packets := func(environment string) int64 {
		state, release := r.lockRecording(environment)
		defer release()
		var total int64
		for _, part := range state.Parts {
			total += part.Packets
		}
		return total
	}
	generateUntil := func(ready func() bool) {
		t.Helper()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for !ready() {
			select {
			case <-ctx.Done():
				t.Fatalf("dumpcap did not record loopback traffic: retention=%+v", r.Retention())
			case <-ticker.C:
			}
			if _, err := udp.WriteTo([]byte("netlab-isolated-capture"), udp.LocalAddr()); err != nil {
				t.Fatal(err)
			}
		}
	}
	generateUntil(func() bool { return packets(one) > 0 && packets(two) > 0 })
	for environment, request := range requests {
		generateUntil(func() bool {
			detail, err := m.Get(request.Id, environment)
			if err != nil {
				t.Fatal(err)
			}
			return detail.Segment.Packets > 0
		})
	}
	if _, err = m.Get(requests[one].Id, two); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-environment capture exposed: %v", err)
	}
	if _, err = m.Stop(ctx, requests[one].Id, one); err != nil {
		t.Fatal(err)
	}
	restarted := requests[one]
	restarted.Id = uuid.NewString()
	if _, err = m.Start(ctx, restarted); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop(ctx, requests[one].Id, one); err != nil {
		t.Fatal(err)
	}
	generateUntil(func() bool {
		detail, err := m.Get(restarted.Id, one)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Segment.Status == api.CaptureSegmentStatusStopped {
			t.Fatal("stopping the old capture stopped its replacement")
		}
		return detail.Segment.Packets > 0
	})
	if _, err = m.Stop(ctx, restarted.Id, one); err != nil {
		t.Fatal(err)
	}
	m.stopRecording(one)
	select {
	case <-m.recordings[two].done:
		t.Fatal("stopping one environment stopped the other capture")
	default:
	}
	before := packets(two)
	generateUntil(func() bool { return packets(two) > before })
	m.stopRecording(two)
	if err = r.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, info := range r.Retention() {
		if info.Error != nil {
			t.Fatal(*info.Error)
		}
	}
}
