//go:build linux

package capture

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"netlab.local/core/api"
)

func TestRealPacketDecoding(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CAPTURE") != "1" {
		t.Skip("set NETLAB_REAL_CAPTURE=1 for native TShark test")
	}
	m := &Manager{directory: t.TempDir(), live: map[string]*session{}}
	id, environment := "0697ab81-7306-4dd5-b151-825fbe28cf4d", "environment"
	if err := os.Mkdir(filepath.Join(m.directory, id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.directory, id, "capture.pcapng"), packetFixture(t), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.save(api.CaptureDetail{Segment: api.CaptureSegment{Id: id, EnvironmentId: environment, Status: "stopped"}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page, err := m.Packets(ctx, id, environment, PacketQuery{Limit: 1, Protocol: "UDP"})
	if err != nil || len(page.Packets) != 1 || page.Packets[0].Number != 1 || page.NextCursor == nil || *page.NextCursor != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, err = m.Packets(ctx, id, environment, PacketQuery{Limit: 1, Protocol: "UDP", After: *page.NextCursor})
	if err != nil || len(page.Packets) != 1 || page.Packets[0].Number != 3 || page.NextCursor != nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	packet, err := m.Packet(ctx, id, environment, 3)
	if err != nil || packet.Hex != hex.EncodeToString(sampleFrame(t)) || packet.Packet.Number != 3 || len(packet.Layers) < 3 {
		t.Fatalf("packet=%+v err=%v", packet, err)
	}
	var udp *api.PacketField
	for index := range packet.Layers {
		if packet.Layers[index].Name == "udp" {
			udp = &packet.Layers[index]
		}
	}
	if udp == nil || udp.Offset != 34 || udp.Length != 8 || len(udp.Children) == 0 {
		t.Fatalf("incorrect protocol bytes: %+v", udp)
	}
	if _, err = m.Packet(ctx, id, environment, 4); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing frame", err)
	}
	if _, err = m.Packets(ctx, id, "another-environment", PacketQuery{Limit: 100}); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cross-environment capture exposed", err)
	}
}
