//go:build linux

package network

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/vishvananda/netlink"
	"netlab.local/core/api"
)

type External struct {
	ovs               *OVS
	nodeID, directory string
}

type externalRecord struct {
	Attachments []api.ExternalAttachment `json:"attachments"`
	Sources     []string                 `json:"sources"`
}

func NewExternal(ctx context.Context, ovs *OVS, directory, nodeID string) (*External, error) {
	e := &External{ovs: ovs, nodeID: nodeID, directory: filepath.Join(directory, "external")}
	if err := os.MkdirAll(e.directory, 0700); err != nil {
		return nil, err
	}
	ovs.mu.Lock()
	defer ovs.mu.Unlock()
	return e, e.reconcile(ctx)
}

func (e *External) Interfaces(ctx context.Context) ([]api.ExternalInterface, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	var bridges []Bridge
	if err = e.ovs.client.List(ctx, &bridges); err != nil {
		return nil, err
	}
	ovsBridges, managed := map[string]bool{}, map[string]bool{}
	for _, b := range bridges {
		ovsBridges[b.Name] = true
		if source := b.ExternalIDs["netlab.external"]; source != "" {
			managed[source] = true
		}
	}
	result := []api.ExternalInterface{}
	for _, link := range links {
		a := link.Attrs()
		if a.Name == e.ovs.bridge || a.Name == ProviderBridge || a.Name == "ovs-system" || strings.HasPrefix(a.Name, "nle") || strings.HasPrefix(a.Name, "nlx") || strings.HasPrefix(a.Name, "nly") || strings.HasPrefix(a.Name, "veth") && a.MasterIndex != 0 && !managed[a.Name] {
			continue
		}
		kind := api.Ethernet
		if ovsBridges[a.Name] {
			kind = api.OvsBridge
		} else if link.Type() == "bridge" {
			kind = api.LinuxBridge
		} else if link.Type() != "device" && link.Type() != "bond" && link.Type() != "veth" && link.Type() != "vlan" {
			continue
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return nil, err
		}
		item := api.ExternalInterface{Name: a.Name, Kind: kind, Mac: a.HardwareAddr.String(), Mtu: a.MTU, Addresses: []string{}, Available: true}
		for _, address := range addresses {
			if address.IP.IsGlobalUnicast() {
				item.Addresses = append(item.Addresses, address.IPNet.String())
			}
		}
		if kind == api.Ethernet && !managed[a.Name] {
			routes, err := netlink.RouteList(link, netlink.FAMILY_ALL)
			if err != nil {
				return nil, err
			}
			item.Available = len(item.Addresses) == 0 && a.MasterIndex == 0 && !slices.ContainsFunc(routes, func(route netlink.Route) bool { return route.Dst == nil || route.Dst.IP.IsGlobalUnicast() })
		}
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b api.ExternalInterface) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

// Persist the desired attachment set before touching the host. The same apply
// restores it after interruption or rolls it back through the parent Operation.
func (e *External) Apply(ctx context.Context, environment string, attachments []api.ExternalAttachment) error {
	e.ovs.mu.Lock()
	defer e.ovs.mu.Unlock()
	for _, attachment := range attachments {
		if attachment.NodeId != e.nodeID {
			return fmt.Errorf("external attachment targets another node")
		}
	}
	interfaces, err := e.Interfaces(ctx)
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if !slices.ContainsFunc(interfaces, func(item api.ExternalInterface) bool { return item.Name == attachment.Interface && item.Available }) {
			return fmt.Errorf("external interface %s is absent or carries host networking", attachment.Interface)
		}
	}
	path := filepath.Join(e.directory, environment+".json")
	record := externalRecord{}
	if raw, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(raw, &record); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	record.Attachments = attachments
	for _, attachment := range attachments {
		if !slices.Contains(record.Sources, attachment.Interface) {
			record.Sources = append(record.Sources, attachment.Interface)
		}
	}
	if err = e.save(path, record); err != nil {
		return err
	}
	if err = e.reconcile(ctx); err != nil {
		return err
	}
	if len(attachments) == 0 {
		return os.Remove(path)
	}
	record.Sources = nil
	for _, attachment := range attachments {
		if !slices.Contains(record.Sources, attachment.Interface) {
			record.Sources = append(record.Sources, attachment.Interface)
		}
	}
	return e.save(path, record)
}

func (e *External) save(path string, record externalRecord) error {
	file, err := os.CreateTemp(e.directory, ".attachment-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	err = json.NewEncoder(file).Encode(record)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func (e *External) reconcile(ctx context.Context) error {
	entries, err := os.ReadDir(e.directory)
	if err != nil {
		return err
	}
	wanted := map[string]map[int]bool{}
	sources := map[string]bool{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(e.directory, entry.Name()))
		if err != nil {
			return err
		}
		var record externalRecord
		if err = json.Unmarshal(raw, &record); err != nil {
			return err
		}
		for _, attachment := range record.Attachments {
			if wanted[attachment.Interface] == nil {
				wanted[attachment.Interface] = map[int]bool{}
			}
			vlan := 0
			if attachment.Vlan != nil {
				vlan = *attachment.Vlan
			}
			wanted[attachment.Interface][vlan] = true
		}
		for _, source := range record.Sources {
			sources[source] = true
		}
	}
	interfaces, err := e.Interfaces(ctx)
	if err != nil {
		return err
	}
	available := map[string]api.ExternalInterface{}
	for _, item := range interfaces {
		available[item.Name] = item
	}
	var roots []OpenVSwitch
	if err = e.ovs.client.List(ctx, &roots); err != nil {
		return err
	}
	if len(roots) != 1 {
		return fmt.Errorf("expected one OVS root")
	}
	root := &roots[0]
	var bridges []Bridge
	if err = e.ovs.client.List(ctx, &bridges); err != nil {
		return err
	}
	mappings := []string{}
	for _, value := range strings.Split(root.ExternalIDs["ovn-bridge-mappings"], ",") {
		if value != "" && !strings.HasPrefix(value, externalNetwork(e.nodeID, "")) {
			mappings = append(mappings, value)
		}
	}
	var operations []ovsdb.Operation
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	slices.Sort(names)
	for index, name := range names {
		item, exists := available[name]
		if !exists || !item.Available {
			return fmt.Errorf("external interface %s is absent or carries host networking", name)
		}
		target := name
		if item.Kind != api.OvsBridge {
			target = externalDevice("nle", name)
			bridge := &Bridge{Name: target}
			if err = e.ovs.client.Get(ctx, bridge); err == client.ErrNotFound {
				link, err := netlink.LinkByName(name)
				if err != nil {
					return err
				}
				device := name
				ids := map[string]string{"netlab.external": name}
				if item.Kind == api.LinuxBridge {
					device = externalDevice("nlx", name)
					peer := externalDevice("nly", name)
					if _, err = netlink.LinkByName(device); err != nil {
						veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: device, MTU: item.Mtu, Alias: "netlab-external:" + name}, PeerName: peer, PeerHardwareAddr: link.Attrs().HardwareAddr}
						if err = netlink.LinkAdd(veth); err != nil {
							return err
						}
						if err = netlink.LinkSetAlias(veth, "netlab-external:"+name); err != nil {
							return errors.Join(err, netlink.LinkDel(veth))
						}
					}
					left, err := netlink.LinkByName(device)
					if err != nil {
						return err
					}
					if left.Attrs().Alias != "netlab-external:"+name {
						return fmt.Errorf("interface %s is not owned by Netlab", device)
					}
					right, err := netlink.LinkByName(peer)
					if err != nil {
						return err
					}
					if err = netlink.LinkSetMaster(right, link); err != nil {
						return err
					}
					if err = netlink.LinkSetUp(right); err != nil {
						return err
					}
					if err = netlink.LinkSetUp(left); err != nil {
						return err
					}
				}
				uid := fmt.Sprintf("external%d", index)
				internal := &Interface{UUID: uid + "internal", Name: target, Type: "internal"}
				internalPort := &Port{UUID: uid + "internalport", Name: target, Interfaces: []string{internal.UUID}}
				uplink := &Interface{UUID: uid + "uplink", Name: device}
				port := &Port{UUID: uid + "port", Name: device, Interfaces: []string{uplink.UUID}}
				bridge = &Bridge{UUID: uid + "bridge", Name: target, Ports: []string{port.UUID, internalPort.UUID}, ExternalIDs: ids}
				created, err := e.ovs.client.Create(internal, internalPort, uplink, port, bridge)
				if err != nil {
					return err
				}
				operations = append(operations, created...)
				root.Bridges = append(root.Bridges, bridge.UUID)
			} else if err != nil {
				return err
			} else if bridge.ExternalIDs["netlab.external"] != name {
				return fmt.Errorf("bridge %s is not owned by Netlab", target)
			}
			if item.Kind == api.LinuxBridge {
				link, err := netlink.LinkByName(name)
				if err != nil {
					return err
				}
				if bridge, ok := link.(*netlink.Bridge); ok && bridge.VlanFiltering != nil && *bridge.VlanFiltering {
					right, err := netlink.LinkByName(externalDevice("nly", name))
					if err != nil {
						return err
					}
					wantedVLANs := map[int]bool{}
					for vlan := range wanted[name] {
						if vlan == 0 {
							vlan = int(*bridge.VlanDefaultPVID)
						}
						wantedVLANs[vlan] = true
					}
					current, err := netlink.BridgeVlanList()
					if err != nil {
						return err
					}
					for _, vlan := range current[int32(right.Attrs().Index)] {
						if !wantedVLANs[int(vlan.Vid)] {
							if err = netlink.BridgeVlanDel(right, vlan.Vid, false, false, false, false); err != nil {
								return err
							}
						}
					}
					for vlan := range wantedVLANs {
						native := wanted[name][0] && vlan == int(*bridge.VlanDefaultPVID)
						if err = netlink.BridgeVlanAdd(right, uint16(vlan), native, native, false, false); err != nil {
							return err
						}
					}
				}
			}
		}
		mappings = append(mappings, externalNetwork(e.nodeID, name)+":"+target)
	}
	for _, bridge := range bridges {
		if name := bridge.ExternalIDs["netlab.external"]; name != "" && wanted[name] == nil {
			root.Bridges = slices.DeleteFunc(root.Bridges, func(id string) bool { return id == bridge.UUID })
			deleted, err := e.ovs.client.Where(&bridge).Delete()
			if err != nil {
				return err
			}
			operations = append(operations, deleted...)
		}
	}
	root.ExternalIDs["ovn-bridge-mappings"] = strings.Join(mappings, ",")
	updated, err := e.ovs.client.Where(root).Update(root, &root.Bridges, &root.ExternalIDs)
	if err != nil {
		return err
	}
	if err = transact(ctx, e.ovs.client, append(operations, updated...)); err != nil {
		return err
	}
	for source := range sources {
		if wanted[source] != nil {
			continue
		}
		link, err := netlink.LinkByName(externalDevice("nlx", source))
		if _, missing := err.(netlink.LinkNotFoundError); missing {
			continue
		}
		if err != nil {
			return err
		}
		if link.Attrs().Alias != "netlab-external:"+source {
			return fmt.Errorf("interface %s is not owned by Netlab", link.Attrs().Name)
		}
		if err = netlink.LinkDel(link); err != nil {
			return err
		}
	}
	return nil
}

// Linux names have a 15-byte limit; the source name remains the ownership key.
func externalDevice(prefix, name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s%x", prefix, sum[:6])
}
