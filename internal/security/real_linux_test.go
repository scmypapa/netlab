//go:build linux

package security

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

func TestRealSuricataOverlappingEnvironments(t *testing.T) {
	if os.Getenv("NETLAB_REAL_SECURITY") != "1" {
		t.Skip("set NETLAB_REAL_SECURITY=1 for native OVS/Suricata verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bridge := "ni" + suffix
	if output, err := exec.CommandContext(ctx, "ovs-vsctl", "add-br", bridge, "--", "set", "Bridge", bridge, "fail_mode=standalone").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	t.Cleanup(func() { exec.Command("ovs-vsctl", "--if-exists", "del-br", bridge).Run() })
	ovs, err := network.NewOVS(ctx, "unix:/run/openvswitch/db.sock", bridge)
	if err != nil {
		t.Fatal(err)
	}
	defer ovs.Close()
	request := api.NodeSecurityRequest{Interfaces: []api.SecurityInterface{}}
	outputs := []netlink.Link{}
	for index, env := range []string{"blue", "red"} {
		device, other := fmt.Sprintf("ni%s%d", suffix, index), fmt.Sprintf("np%s%d", suffix, index)
		link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: device}, PeerName: other}
		if err = netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { netlink.LinkDel(link) })
		if err = netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
		peerLink, err := netlink.LinkByName(other)
		if err != nil {
			t.Fatal(err)
		}
		if err = netlink.LinkSetUp(peerLink); err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, peerLink)
		port := uuid.NewString()
		if err = ovs.Attach(ctx, device, port, env, "web", uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.CommandContext(ctx, "ovs-vsctl", "set", "Port", device, fmt.Sprintf("tag=%d", 100+index)).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, output)
		}
		target := peer(env, "web", port, "one")
		request.Interfaces = append(request.Interfaces, target)
	}
	directory := t.TempDir()
	rules := filepath.Join(directory, "verification.rules")
	if err = os.WriteFile(rules, []byte(`alert udp any any -> any any (msg:"Netlab live verification"; content:"netlab-suricata-verification"; sid:1000001; rev:1; priority:1;)`), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, ovs, directory, "one", "/etc/suricata/suricata.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m.rules = rules
	defer m.Close()
	frame := gopacket.NewSerializeBuffer()
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolUDP, SrcIP: []byte{10, 0, 0, 2}, DstIP: []byte{10, 0, 0, 3}}
	udp := &layers.UDP{SrcPort: 40000, DstPort: 80}
	udp.SetNetworkLayerForChecksum(ip)
	if err = gopacket.SerializeLayers(frame, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true},
		&layers.Ethernet{SrcMAC: []byte{2, 0, 0, 0, 0, 2}, DstMAC: []byte{2, 0, 0, 0, 0, 3}, EthernetType: layers.EthernetTypeIPv4}, ip, udp, gopacket.Payload("netlab-suricata-verification")); err != nil {
		t.Fatal(err)
	}
	protocol := binary.NativeEndian.Uint16([]byte{0, 3})
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(protocol))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	seen := map[string]bool{}
	for ctx.Err() == nil {
		for _, link := range outputs {
			if err = unix.Sendto(fd, frame.Bytes(), 0, &unix.SockaddrLinklayer{Ifindex: link.Attrs().Index}); err != nil {
				t.Fatal(err)
			}
		}
		result, err := m.Poll(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		request.Cursor = result.Cursor
		if result.Sensor.Status == "failed" || result.Sensor.Status == "unavailable" {
			t.Fatalf("sensor: %+v", result.Sensor)
		}
		for _, alert := range result.Alerts {
			if alert.SignatureId != 1000001 || alert.SourceAssetId != "web" || alert.Severity != 1 {
				t.Fatalf("wrong native alert: %+v", alert)
			}
			seen[alert.EnvironmentId] = true
		}
		if seen["blue"] && seen["red"] && result.Sensor.Status == "ready" {
			request.Interfaces = append(request.Interfaces, peer("blue", "missing", "missing-port", "one"))
			partial, err := m.Poll(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if partial.Sensor.Status != "loading" || partial.Sensor.Error == nil || !strings.Contains(*partial.Sensor.Error, "1 个资产接口") {
				t.Fatalf("partial coverage reported ready: %+v", partial.Sensor)
			}
			request.Cursor = partial.Cursor
			request.Interfaces = nil
			for stop := time.Now().Add(5 * time.Second); time.Now().Before(stop); {
				result, err = m.Poll(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				request.Cursor = result.Cursor
				if result.Sensor.Status == "idle" {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("sensor did not stop when the last asset was removed")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("native detection not complete: %v", seen)
}
