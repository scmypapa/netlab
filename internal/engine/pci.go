package engine

import (
	"fmt"
	"strconv"
	"strings"

	"libvirt.org/go/libvirtxml"
)

func domainHostDevices(addresses []string) ([]libvirtxml.DomainHostdev, error) {
	result := []libvirtxml.DomainHostdev{}
	for _, address := range addresses {
		parts := strings.Split(strings.ReplaceAll(address, ".", ":"), ":")
		if len(parts) != 4 || len(parts[0]) != 4 || len(parts[1]) != 2 || len(parts[2]) != 2 || len(parts[3]) != 1 || address[10] != '.' {
			return nil, fmt.Errorf("invalid PCI address %s", address)
		}
		values := make([]uint, 4)
		for i, part := range parts {
			value, err := strconv.ParseUint(part, 16, 16)
			if err != nil {
				return nil, err
			}
			values[i] = uint(value)
		}
		if values[1] > 255 || values[2] > 31 || values[3] > 7 {
			return nil, fmt.Errorf("invalid PCI address %s", address)
		}
		result = append(result, libvirtxml.DomainHostdev{Managed: "yes", SubsysPCI: &libvirtxml.DomainHostdevSubsysPCI{Source: &libvirtxml.DomainHostdevSubsysPCISource{Address: &libvirtxml.DomainAddressPCI{Domain: &values[0], Bus: &values[1], Slot: &values[2], Function: &values[3]}}}})
	}
	return result, nil
}
