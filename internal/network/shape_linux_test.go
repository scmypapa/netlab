//go:build linux

package network

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
	"netlab.local/core/api"
)

func TestRealBidirectionalShape(t *testing.T) {
	if os.Getenv("NETLAB_REAL_IMAGE") == "" {
		t.Skip("enable local execution tests")
	}
	port := uuid.NewString()
	name := "nt" + strings.ReplaceAll(port, "-", "")[:12]
	device := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: 1400}}
	if err := netlink.LinkAdd(device); err != nil {
		t.Fatal(err)
	}
	defer netlink.LinkDel(device)
	defer ClearShape(name, port)
	if err := netlink.LinkSetUp(device); err != nil {
		t.Fatal(err)
	}
	delay, rate, loss, protocol := 12, 3000, float32(2), "tcp"
	policy := api.Policy{Id: "limited", Direction: api.Both, Action: api.Shape, DelayMs: &delay, BandwidthKbps: &rate, LossPercent: &loss, Protocol: &protocol}
	if err := Shape(name, port, []api.Policy{policy}); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(ifbName(port)); err != nil {
		t.Fatal("egress IFB was not created", err)
	}
	if err := Shape(name, port, []api.Policy{policy}); err != nil {
		t.Fatal("repeat identical policy", err)
	}
	if err := Shape(name, port, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(ifbName(port)); err == nil {
		t.Fatal("IFB remained after policy removal")
	}
}
