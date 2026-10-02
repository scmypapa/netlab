//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"netlab.local/core/api"
)

func TestRealInitializationMedia(t *testing.T) {
	if os.Getenv("NETLAB_REAL_VM_IMPORTS") == "" {
		t.Skip("set NETLAB_REAL_VM_IMPORTS to test the actual ISO tools")
	}
	for _, method := range []api.TemplateInitialization{api.CloudInit, api.CloudbaseInit} {
		t.Run(string(method), func(t *testing.T) {
			directory := t.TempDir()
			a := api.AssetExecution{InstanceId: "stable-instance", Template: api.Template{Initialization: &method}, Interfaces: []api.ResolvedInterface{{Id: "primary", Mac: "02:12:13:14:15:16", Address: "192.168.18.20", Prefix: 24, Mtu: 1400, Primary: true, Gateway: ptr("192.168.18.1")}}}
			media, err := stageInitialization(context.Background(), directory, a)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(filepath.Dir(media))
			info, err := exec.Command("isoinfo", "-d", "-i", media).CombinedOutput()
			label, metadataPath, networkPath, instanceKey := "cidata", "/meta-data", "/network-config", "instance-id"
			if method == api.CloudbaseInit {
				label, metadataPath, networkPath, instanceKey = "config-2", "/openstack/latest/meta_data.json", "/openstack/latest/network_data.json", "uuid"
			}
			if err != nil || !strings.Contains(string(info), "Volume id: "+label) {
				t.Fatalf("ISO label: %s %v", info, err)
			}
			data, err := exec.Command("isoinfo", "-R", "-i", media, "-x", metadataPath).CombinedOutput()
			var metadata map[string]any
			if err != nil || json.Unmarshal(data, &metadata) != nil || metadata[instanceKey] != a.InstanceId {
				t.Fatalf("ISO metadata: %s %v", data, err)
			}
			data, err = exec.Command("isoinfo", "-R", "-i", media, "-x", networkPath).CombinedOutput()
			if err != nil || !strings.Contains(string(data), a.Interfaces[0].Mac) || !strings.Contains(string(data), a.Interfaces[0].Address) {
				t.Fatalf("ISO network data: %s %v", data, err)
			}
			t.Logf("%s: actual %s ISO includes stable identity and MAC-bound network data", method, label)
		})
	}
}
