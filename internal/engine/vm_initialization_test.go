package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"netlab.local/core/api"
)

func TestStandardGuestInitialization(t *testing.T) {
	pointer := func(value string) *string { return &value }
	interfaces := []api.ResolvedInterface{
		{Id: "business", Mac: "02:00:12:00:00:01", Address: "192.168.12.20", Prefix: 24, Mtu: 1400, Primary: true, Gateway: pointer("192.168.12.1"), Dns: &[]string{"192.168.12.10"}},
		{Id: "storage", Mac: "02:00:13:00:00:01", Address: "192.168.13.20", Prefix: 24, Mtu: 1400, Gateway: pointer("192.168.13.1"), Dns: &[]string{"192.168.13.10"}},
	}
	read := func(seed guestSeed, path string) map[string]any {
		t.Helper()
		var result map[string]any
		data := strings.TrimPrefix(string(seed.files[path]), "#cloud-config\n")
		if err := json.Unmarshal([]byte(data), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, method := range []api.TemplateInitialization{api.CloudInit, api.CloudbaseInit} {
		t.Run(string(method), func(t *testing.T) {
			a := api.AssetExecution{InstanceId: "unchanged-instance", Template: api.Template{Initialization: &method}, Interfaces: interfaces}
			seed, err := initializationSeed(a)
			if err != nil {
				t.Fatal(err)
			}
			if method == api.CloudInit {
				metadata, network, user := read(seed, "meta-data"), read(seed, "network-config"), read(seed, "user-data")
				if metadata["instance-id"] != a.InstanceId || metadata["local-hostname"] != nil || user["preserve_hostname"] != true || user["users"] != nil {
					t.Fatal("unset guest identity was modified")
				}
				updates := user["updates"].(map[string]any)["network"].(map[string]any)["when"].([]any)
				if len(updates) != 1 || updates[0] != "boot" {
					t.Fatal("network updates would reuse the first-boot cache")
				}
				devices := network["ethernets"].(map[string]any)
				primary, secondary := devices["eth0"].(map[string]any), devices["eth1"].(map[string]any)
				if primary["match"].(map[string]any)["macaddress"] != interfaces[0].Mac || primary["routes"] == nil || secondary["routes"] != nil || secondary["nameservers"].(map[string]any)["addresses"].([]any)[0] != "192.168.13.10" {
					t.Fatal("MAC, per-interface DNS or primary-only route was lost")
				}
			} else {
				metadata, network := read(seed, "openstack/latest/meta_data.json"), read(seed, "openstack/latest/network_data.json")
				if seed.label != "config-2" || metadata["uuid"] != a.InstanceId || metadata["hostname"] != nil || metadata["meta"] != nil {
					t.Fatal("unset Windows guest identity was modified")
				}
				links, networks := network["links"].([]any), network["networks"].([]any)
				if links[0].(map[string]any)["ethernet_mac_address"] != interfaces[0].Mac || networks[0].(map[string]any)["routes"] == nil || networks[1].(map[string]any)["routes"] != nil || networks[1].(map[string]any)["services"].([]any)[0].(map[string]any)["address"] != "192.168.13.10" {
					t.Fatal("Windows MAC, per-interface DNS or primary-only route was lost")
				}
			}
			a.Asset.Guest = &api.GuestSettings{Hostname: pointer("test-guest"), Username: pointer("tester"), SshAuthorizedKeys: &[]string{"ssh-ed25519 public-test-key"}}
			seed, err = initializationSeed(a)
			if err != nil {
				t.Fatal(err)
			}
			if method == api.CloudInit {
				user := read(seed, "user-data")
				account := user["users"].([]any)[0].(map[string]any)
				if user["hostname"] != "test-guest" || account["name"] != "tester" || len(account["ssh_authorized_keys"].([]any)) != 1 || user["ssh_authorized_keys"] != nil {
					t.Fatal("explicit Linux user and key were not assigned together")
				}
			} else {
				metadata := read(seed, "openstack/latest/meta_data.json")
				if metadata["hostname"] != "test-guest" || metadata["meta"].(map[string]any)["admin_username"] != "tester" || len(metadata["public_keys"].(map[string]any)) != 1 {
					t.Fatal("explicit Windows user and key were not assigned together")
				}
			}
		})
	}
	a := api.AssetExecution{Template: api.Template{}}
	seed, err := initializationSeed(a)
	if err != nil || len(seed.files) != 0 {
		t.Fatal("static template produced initialization media")
	}
	a.Asset.Guest = &api.GuestSettings{Username: pointer("tester")}
	if _, err = initializationSeed(a); err == nil {
		t.Fatal("static template silently ignored guest settings")
	}
}
