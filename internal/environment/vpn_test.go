package environment

import (
	"netlab.local/core/api"
	"testing"
)

func TestVPNOverlappingNetworkAddressModes(t *testing.T) {
	networks := map[string]api.Network{"left": {Id: "left", Name: "Left", Cidr: "10.0.0.0/24"}, "right": {Id: "right", Name: "Right", Cidr: "10.0.0.0/24"}}
	routes := []api.VPNRoute{{NetworkId: "left"}, {NetworkId: "right"}}
	if mode, err := vpnAddressMode(nil, routes, networks); err != nil || mode != api.Translated {
		t.Fatal("unspecified mode did not map overlapping networks", mode, err)
	}
	original := api.Original
	if _, err := vpnAddressMode(&original, routes, networks); err == nil {
		t.Fatal("explicit original mode accepted ambiguous destination addresses")
	}
	translated := api.Translated
	if mode, err := vpnAddressMode(&translated, routes, networks); err != nil || mode != api.Translated {
		t.Fatal(mode, err)
	}
	if mode, err := vpnAddressMode(&original, routes[:1], networks); err != nil || mode != api.Original {
		t.Fatal("unambiguous original mode changed", mode, err)
	}
}
