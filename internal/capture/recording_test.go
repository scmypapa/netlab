package capture

import (
	"bytes"
	"context"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"netlab.local/core/api"
)

func recordingFixture(t *testing.T, quota int64) (*Recorder, string, []api.CaptureInterface) {
	t.Helper()
	node, environment := uuid.NewString(), uuid.NewString()
	r, err := NewRecorder(t.TempDir(), node)
	if err != nil {
		t.Fatal(err)
	}
	interfaces := []api.CaptureInterface{{AssetId: "vm", AssetName: "VM", InterfaceId: "nic", NetworkId: "lan", PortName: uuid.NewString(), NodeId: node, Mac: "02:00:00:00:00:01", Address: "192.0.2.1"}}
	if err = r.configure(map[string]int64{environment: quota}, []api.SecurityInterface{{EnvironmentId: environment, Interface: interfaces[0]}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, environment, interfaces
}
func TestCompressedRecorderPreservesFramesAndEnvironmentIdentity(t *testing.T) {
	r, environment, interfaces := recordingFixture(t, 1<<20)
	other := uuid.NewString()
	otherInterfaces := slicesCloneInterfaces(interfaces)
	otherInterfaces[0].AssetId = "other-vm"
	otherInterfaces[0].PortName = uuid.NewString()
	if err := r.configure(map[string]int64{environment: 1 << 20, other: 1 << 20}, []api.SecurityInterface{{EnvironmentId: environment, Interface: interfaces[0]}, {EnvironmentId: other, Interface: otherInterfaces[0]}}); err != nil {
		t.Fatal(err)
	}
	frame := sampleFrame(t)
	at := time.Unix(1791115200, 0)
	for index, id := range []string{environment, other} {
		ifaces := interfaces
		if index == 1 {
			ifaces = otherInterfaces
		}
		if err := r.Record(id, ifaces, 0, gopacket.CaptureInfo{Timestamp: at, Length: len(frame), CaptureLength: len(frame)}, frame); err != nil {
			t.Fatal(err)
		}
	}
	for _, sealed := range []bool{false, true} {
		if sealed {
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []string{environment, other} {
			count := 0
			err := r.walk(context.Background(), id, func(_ packetPart, packet api.CapturePacket, data []byte) bool {
				count++
				if !bytes.Equal(data, frame) || packet.Id == nil || packet.NetworkId == nil || *packet.NetworkId != "lan" {
					t.Fatal(packet)
				}
				expected := "vm"
				if id == other {
					expected = "other-vm"
				}
				if packet.AssetId == nil || *packet.AssetId != expected {
					t.Fatalf("cross-environment frame: %+v", packet)
				}
				return true
			})
			if err != nil || count != 1 {
				t.Fatalf("sealed=%v count=%d err=%v", sealed, count, err)
			}
		}
	}
	if err := r.Remove(environment); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.directory, environment)); !os.IsNotExist(err) {
		t.Fatal("environment files remain", err)
	}
	if _, ok := r.Retention()[other]; !ok {
		t.Fatal("another environment removed")
	}
}
func slicesCloneInterfaces(input []api.CaptureInterface) []api.CaptureInterface {
	return append([]api.CaptureInterface{}, input...)
}
func TestRecorderCompressedQuotaRollsOldestAndSurvivesRestart(t *testing.T) {
	r, environment, interfaces := recordingFixture(t, 32<<10)
	random := rand.New(rand.NewSource(1))
	frame := make([]byte, 4096)
	at := time.Unix(1791115200, 0)
	for index := 0; index < 30; index++ {
		random.Read(frame)
		if err := r.Record(environment, interfaces, 0, gopacket.CaptureInfo{Timestamp: at.Add(time.Duration(index) * 31 * time.Second), Length: len(frame), CaptureLength: len(frame)}, frame); err != nil {
			t.Fatal(err)
		}
		if err := r.Flush(); err != nil {
			t.Fatal(err)
		}
		if r.Retention()[environment].UsedBytes > 32<<10 {
			t.Fatal("quota exceeded", r.Retention())
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	retained := r.Retention()[environment]
	if retained.UsedBytes == 0 || retained.OldestAt == nil || !retained.OldestAt.After(at) {
		t.Fatal("oldest parts were not evicted", retained)
	}
	restored, err := NewRecorder(filepath.Dir(r.directory), r.node)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.Retention()[environment].UsedBytes != retained.UsedBytes {
		t.Fatal("retention changed after restart")
	}
	files, _ := filepath.Glob(filepath.Join(r.directory, environment, "*.pcapng.zst"))
	var size int64
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		size += info.Size()
	}
	if size != retained.UsedBytes || size > 32<<10 {
		t.Fatalf("actual=%d metadata=%d", size, retained.UsedBytes)
	}
}
func TestRecentPacketQueryAndStableOlderPagination(t *testing.T) {
	q, err := ParseRecentQuery(url.Values{"source": {"02:00:00:00:00:01"}, "destination": {"192.0.2.2"}, "sourcePort": {"502"}, "filter": {"tcp.flags.syn == 1"}, "start": {"2026-10-23T00:00:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	filter := q.display(packetPart{})
	for _, expected := range []string{"eth.src == 02:00:00:00:00:01", "ip.dst == 192.0.2.2", "tcp.flags.syn == 1", "frame.time_epoch"} {
		if !strings.Contains(filter, expected) {
			t.Fatal(filter)
		}
	}
	for _, values := range []url.Values{{"source": {"not-an-address"}}, {"end": {"bad"}}, {"cursor": {"bad"}}, {"start": {"2026-10-23T01:00:00Z"}, "end": {"2026-10-23T00:00:00Z"}}} {
		if _, err = ParseRecentQuery(values); err == nil {
			t.Fatal("invalid query accepted", values)
		}
	}
	packets := []api.CapturePacket{}
	for index := 0; index < 4; index++ {
		id := packetIdentity(uuid.NewString(), uuid.NewString(), 1)
		packets = append(packets, api.CapturePacket{Id: &id, Time: time.Unix(int64(index), 0)})
	}
	merged := MergePacketPages([]api.RecentPacketPage{{Packets: packets[:2]}, {Packets: packets[2:]}}, 2)
	if len(merged.Packets) != 2 || !merged.Packets[0].Time.Equal(packets[2].Time) || merged.NextCursor == nil {
		t.Fatal(merged)
	}
	cursor, err := parsePacketCursor(*merged.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	for index, packet := range packets {
		before := packetBefore(packet, cursor)
		if before != (index < 2) {
			t.Fatalf("index=%d before=%v", index, before)
		}
	}
	unavailable := api.RecentPacketPage{Retention: api.PacketRetention{Status: api.PacketRetentionStatusUnavailable}}
	recording := api.RecentPacketPage{Retention: api.PacketRetention{Status: api.PacketRetentionStatusRecording}}
	for _, pages := range [][]api.RecentPacketPage{{unavailable, recording}, {recording, unavailable}} {
		if MergePacketPages(pages, 2).Retention.Status != api.PacketRetentionStatusUnavailable {
			t.Fatal("recording node hides unavailable peer")
		}
	}
}

func TestRecorderRecoversAndSealedReadsDoNotWrite(t *testing.T) {
	r, environment, interfaces := recordingFixture(t, 1<<20)
	r.environments[environment].Error = "temporary write failure"
	frame := sampleFrame(t)
	if err := r.Record(environment, interfaces, 0, gopacket.CaptureInfo{Timestamp: time.Now(), Length: len(frame), CaptureLength: len(frame)}, frame); err != nil {
		t.Fatal(err)
	}
	if r.Retention()[environment].Error != nil {
		t.Fatal("recovered recorder retains an error")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(r.directory, environment, "manifest.json")
	at := time.Unix(1791115200, 0)
	if err := os.Chtimes(manifest, at, at); err != nil {
		t.Fatal(err)
	}
	if err := r.walk(context.Background(), environment, func(packetPart, api.CapturePacket, []byte) bool { return true }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(manifest)
	if err != nil || !info.ModTime().Equal(at) {
		t.Fatal("reading sealed packets rewrote the manifest", err)
	}
}
