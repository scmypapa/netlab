//go:build linux

package capture

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

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
	m := &Manager{ctx: ctx, node: node, recorder: r, recordings: map[string]*recordingRun{}, live: map[string]*session{}}
	defer func() {
		for environment := range m.recordings {
			m.stopRecording(environment)
		}
	}()
	if err = r.configure(map[string]int64{one: 1 << 20, two: 1 << 20}, nil); err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{one, two} {
		iface := api.CaptureInterface{NodeId: node, AssetId: "guest", AssetName: "Guest", InterfaceId: "nic", PortName: environment, NetworkId: "lan"}
		if err = m.startRecording(environment, []api.SecurityInterface{{EnvironmentId: environment, Interface: iface}}, map[string]string{environment: "lo"}); err != nil {
			t.Fatal(err)
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
				t.Fatal("dumpcap did not record loopback traffic")
			case <-ticker.C:
			}
			if _, err := udp.WriteTo([]byte("netlab-isolated-capture"), udp.LocalAddr()); err != nil {
				t.Fatal(err)
			}
		}
	}
	generateUntil(func() bool { return packets(one) > 0 && packets(two) > 0 })
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
