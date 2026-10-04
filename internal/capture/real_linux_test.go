//go:build linux

package capture

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

func TestRealMirrorCaptureLifecycle(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CAPTURE") != "1" {
		t.Skip("set NETLAB_REAL_CAPTURE=1 for native OVS/tshark test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := uuid.NewString()
	suffix := strings.ReplaceAll(id, "-", "")[:10]
	bridge, device, peer := "ct"+suffix, "cs"+suffix, "cp"+suffix
	command := func(args ...string) {
		t.Helper()
		if output, err := exec.CommandContext(ctx, "ovs-vsctl", args...).CombinedOutput(); err != nil {
			t.Fatalf("OVS: %v %s", err, output)
		}
	}
	command("add-br", bridge, "--", "set", "Bridge", bridge, "fail_mode=standalone")
	t.Cleanup(func() { exec.Command("ovs-vsctl", "--if-exists", "del-br", bridge).Run() })
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: device}, PeerName: peer}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMTU(link, 9000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netlink.LinkDel(link) })
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	other, err := netlink.LinkByName(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMTU(other, 9000); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(other); err != nil {
		t.Fatal(err)
	}
	logicalPort := uuid.NewString()
	command("add-port", bridge, device, "--", "set", "Interface", device, "external_ids:iface-id="+logicalPort)
	ovs, err := network.NewOVS(ctx, "unix:/run/openvswitch/db.sock", bridge)
	if err != nil {
		t.Fatal(err)
	}
	defer ovs.Close()
	directory := t.TempDir()
	m, err := New(ctx, ovs, directory, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	request := api.NodeCaptureRequest{Id: id, EnvironmentId: "environment", Settings: api.CreateCapture{AssetIds: []string{"client"}, DurationSeconds: 10, FileSizeMiB: 1}, Interfaces: []api.CaptureInterface{
		{AssetId: "client", NodeId: "one", PortName: logicalPort, Mac: "02:00:00:00:00:01", Address: "192.0.2.1"},
		{AssetId: "server", NodeId: "two", Mac: "02:00:00:00:00:02", Address: "192.0.2.2"},
	}}
	if _, err := m.Start(ctx, request); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	frame := testFrame(8192)
	var detail api.CaptureDetail
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); {
		if err := unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: other.Attrs().Index, Protocol: htons(unix.ETH_P_IP)}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		detail, err = m.Get(id, request.EnvironmentId)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Segment.Status == api.CaptureSegmentStatusFailed {
			t.Fatalf("native capture failed: %s", *detail.Segment.Error)
		}
		if len(detail.Flows) > 0 {
			break
		}
	}
	if len(detail.Flows) != 1 || detail.Flows[0].Protocol != "UDP" || *detail.Flows[0].SourceAssetId != "client" || *detail.Flows[0].DestinationAssetId != "server" {
		t.Fatalf("native packet missing: %+v", detail)
	}
	segment, err := m.Stop(ctx, id, request.EnvironmentId)
	if err != nil || segment.Status != api.CaptureSegmentStatusStopped {
		if segment.Error != nil {
			t.Log(*segment.Error)
		}
		t.Fatalf("stop: %+v %v", segment, err)
	}
	file, err := os.Open(filepath.Join(directory, "captures", id, "capture.pcapng"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := file.Stat(); err != nil || info.Size() < 8192 {
		t.Fatalf("jumbo frame absent from PCAP: %v %v", info, err)
	}
	magic := make([]byte, 4)
	_, err = file.Read(magic)
	file.Close()
	if err != nil || binary.LittleEndian.Uint32(magic) != 0x0a0d0d0a {
		t.Fatalf("not a PCAPNG: %x %v", magic, err)
	}
	reloaded, err := New(ctx, ovs, directory, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	detail, err = reloaded.Get(id, request.EnvironmentId)
	if err != nil || len(detail.Flows) != 1 || detail.Segment.Status != api.CaptureSegmentStatusStopped {
		t.Fatalf("restart: %+v %v", detail, err)
	}
	if err := reloaded.RemoveEnvironment(ctx, request.EnvironmentId); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(directory, "captures")); err != nil || len(entries) != 0 {
		t.Fatalf("capture residue: %v %v", entries, err)
	}
	mirrorDevice, _ := deviceNames(id)
	if _, err := netlink.LinkByName(mirrorDevice); err == nil {
		t.Fatal("mirror interface remains")
	}
	output, err := exec.Command("ovs-vsctl", "--columns=name", "--format=csv", "--data=bare", "--no-headings", "find", "Mirror", "external_ids:netlab.capture="+id).Output()
	if err != nil || len(strings.TrimSpace(string(output))) != 0 {
		t.Fatalf("OVS mirror residue: %s %v", output, err)
	}
}

func htons(value uint16) uint16 { return value<<8 | value>>8 }
func testFrame(size int) []byte {
	frame := make([]byte, size)
	copy(frame[:6], []byte{2, 0, 0, 0, 0, 2})
	copy(frame[6:12], []byte{2, 0, 0, 0, 0, 1})
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_IP)
	ip := frame[14:34]
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(ip[2:4], uint16(size-14))
	copy(ip[12:16], []byte{192, 0, 2, 1})
	copy(ip[16:20], []byte{192, 0, 2, 2})
	var sum uint32
	for index := 0; index < len(ip); index += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[index : index+2]))
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(ip[10:12], ^uint16(sum))
	udp := frame[34:]
	binary.BigEndian.PutUint16(udp[:2], 50000)
	binary.BigEndian.PutUint16(udp[2:4], 9000)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], "netlab capture test")
	return frame
}
