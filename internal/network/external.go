package network

import "strings"

func externalNetwork(nodeID, name string) string {
	return "netlab-external-" + strings.ReplaceAll(nodeID, "-", "") + "-" + name
}
