package engine

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"

	"netlab.local/core/api"
)

type guestSeed struct {
	label string
	files map[string][]byte
}

func initializationMethod(t api.Template) api.TemplateInitialization {
	if t.Initialization == nil {
		return api.None
	}
	return *t.Initialization
}

func initializationSeed(a api.AssetExecution) (guestSeed, error) {
	seed := guestSeed{files: map[string][]byte{}}
	method := initializationMethod(a.Template)
	if method == api.None {
		if a.Asset.Guest != nil {
			return seed, fmt.Errorf("template %s does not declare a guest initialization method", a.Template.Name)
		}
		return seed, nil
	}
	metadata := map[string]any{}
	userData := map[string]any{}
	if method == api.CloudInit {
		seed.label = "cidata"
		metadata["instance-id"], metadata["dsmode"] = a.InstanceId, "local"
		userData["preserve_hostname"] = true
		// Re-read only network configuration on boot; identity modules stay per-instance.
		userData["updates"] = map[string]any{"network": map[string]any{"when": []string{"boot"}}}
	} else if method == api.CloudbaseInit {
		seed.label = "config-2"
		metadata["uuid"] = a.InstanceId
	} else {
		return seed, fmt.Errorf("unsupported guest initialization method %s", method)
	}
	if guest := a.Asset.Guest; guest != nil {
		if guest.Hostname != nil {
			if method == api.CloudInit {
				metadata["local-hostname"], userData["hostname"] = *guest.Hostname, *guest.Hostname
				userData["preserve_hostname"] = false
			} else {
				metadata["hostname"] = *guest.Hostname
			}
		}
		if method == api.CloudInit {
			if guest.Username != nil {
				user := map[string]any{"name": *guest.Username, "lock_passwd": true}
				if guest.SshAuthorizedKeys != nil {
					user["ssh_authorized_keys"] = *guest.SshAuthorizedKeys
				}
				userData["users"] = []any{user}
			} else if guest.SshAuthorizedKeys != nil {
				userData["ssh_authorized_keys"] = *guest.SshAuthorizedKeys
			}
		} else {
			if guest.Username != nil {
				metadata["meta"] = map[string]string{"admin_username": *guest.Username}
			}
			if guest.SshAuthorizedKeys != nil {
				keys := map[string]string{}
				for index, key := range *guest.SshAuthorizedKeys {
					keys[fmt.Sprintf("key%d", index)] = key
				}
				metadata["public_keys"] = keys
			}
		}
	}
	ethernet := map[string]any{}
	links, networks := []any{}, []any{}
	for index, iface := range a.Interfaces {
		address, err := netip.ParseAddr(iface.Address)
		if err != nil {
			return seed, fmt.Errorf("interface %s address: %w", iface.Id, err)
		}
		name := fmt.Sprintf("eth%d", index)
		maskBits, family, defaultRoute := 128, "ipv6", "::/0"
		if address.Is4() {
			maskBits, family, defaultRoute = 32, "ipv4", "0.0.0.0/0"
		}
		mask := net.CIDRMask(iface.Prefix, maskBits)
		if mask == nil {
			return seed, fmt.Errorf("interface %s has an invalid prefix", iface.Id)
		}
		if method == api.CloudInit {
			config := map[string]any{"match": map[string]string{"macaddress": iface.Mac}, "set-name": name, "addresses": []string{fmt.Sprintf("%s/%d", iface.Address, iface.Prefix)}, "mtu": iface.Mtu, "dhcp4": false, "dhcp6": false}
			if iface.Dns != nil {
				config["nameservers"] = map[string]any{"addresses": *iface.Dns}
			}
			if iface.Primary && iface.Gateway != nil {
				config["routes"] = []any{map[string]string{"to": defaultRoute, "via": *iface.Gateway}}
			}
			ethernet[name] = config
		} else {
			links = append(links, map[string]any{"id": name, "name": name, "type": "phy", "ethernet_mac_address": iface.Mac, "mtu": iface.Mtu})
			network := map[string]any{"id": name + "-network", "link": name, "type": family, "ip_address": iface.Address, "netmask": net.IP(mask).String()}
			if iface.Dns != nil {
				services := []any{}
				for _, server := range *iface.Dns {
					services = append(services, map[string]string{"type": "dns", "address": server})
				}
				network["services"] = services
			}
			if iface.Primary && iface.Gateway != nil {
				zero := net.IP(make([]byte, maskBits/8)).String()
				network["routes"] = []any{map[string]string{"network": zero, "netmask": zero, "gateway": *iface.Gateway}}
			}
			networks = append(networks, network)
		}
	}
	data := map[string]any{}
	if method == api.CloudInit {
		data["meta-data"] = metadata
		data["network-config"] = map[string]any{"version": 2, "ethernets": ethernet}
		data["user-data"] = userData
	} else {
		data["openstack/latest/meta_data.json"] = metadata
		data["openstack/latest/network_data.json"] = map[string]any{"links": links, "networks": networks, "services": []any{}}
		data["openstack/latest/user_data"] = ""
	}
	for path, content := range data {
		value, err := json.MarshalIndent(content, "", "  ")
		if err != nil {
			return seed, err
		}
		if path == "user-data" {
			value = append([]byte("#cloud-config\n"), value...)
		} else if path == "openstack/latest/user_data" {
			value = nil
		}
		seed.files[path] = value
	}
	return seed, nil
}
