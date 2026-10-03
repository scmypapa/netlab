package network

import (
	"slices"
	"testing"

	"netlab.local/core/api"
)

func TestVPNReplayRestoresAddressesAfterReuse(t *testing.T) {
	old := []string{"198.18.0.3/32", "fdff::3/128"}
	current := accessRecord{Clients: []string{"198.18.0.0/20", "fdff::/64"}, Peers: []vpnPeer{
		{Peer: api.VPNPeer{Id: "survivor"}, Addresses: []string{"198.18.0.4/32", "fdff::4/128"}},
		{Peer: api.VPNPeer{Id: "new-client"}, Addresses: old},
	}}
	desired := []api.VPNPeer{{Id: "restored", Addresses: &old}, {Id: "survivor"}}
	peers, err := assignVPNPeers(current, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(peers[0].Addresses, old) || !slices.Equal(peers[1].Addresses, current.Peers[0].Addresses) {
		t.Fatal("rollback changed an existing client's downloaded configuration")
	}
	if peers[0].Peer.Addresses != nil {
		t.Fatal("applied addresses were also kept in the desired definition")
	}
	if _, err = assignVPNPeers(current, append(desired, api.VPNPeer{Id: "duplicate", Addresses: &old})); err == nil {
		t.Fatal("two current clients received the same source address")
	}
}

func TestVPNNetworkUpdateConflictsOnlyWithAllocatedRanges(t *testing.T) {
	record := accessRecord{Transit: []string{"198.18.16.0/30", "fdff:0:0:1::/64"}, Clients: []string{"198.18.0.0/20", "fdff::/64"}}
	plan := api.NodePlan{Spec: api.EnvironmentSpec{Networks: []api.Network{{Name: "ordinary", Cidr: "10.18.0.0/24"}}}}
	if err := vpnRangeConflict(record, plan); err != nil {
		t.Fatal(err)
	}
	plan.Spec.Networks = append(plan.Spec.Networks, api.Network{Name: "overlapping", Cidr: "198.18.4.0/24"})
	if err := vpnRangeConflict(record, plan); err == nil {
		t.Fatal("network update silently invalidated an active VPN address pool")
	}
}

func TestEnvironmentAccessReallocatesUnusedInternalRanges(t *testing.T) {
	record := accessRecord{Transit: []string{"198.18.16.0/30", "fdff:0:0:1::/64"}, Clients: []string{"198.18.0.0/20", "fdff::/64"}}
	plan := api.NodePlan{Spec: api.EnvironmentSpec{Networks: []api.Network{{Id: "lan", Name: "LAN", Cidr: "198.18.16.0/24"}}}}
	desired, err := accessNetwork(record, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = vpnRangeConflict(desired, plan); err != nil {
		t.Fatal(err)
	}
	if slices.Equal(record.Transit, desired.Transit) {
		t.Fatal("internal transit did not move out of the target LAN")
	}
	record.Peers = []vpnPeer{{Peer: api.VPNPeer{Id: "active"}}}
	if _, err = accessNetwork(record, plan); err == nil {
		t.Fatal("active VPN addressing changed without updating client configuration")
	}
}
