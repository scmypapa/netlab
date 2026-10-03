package environment

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"netlab.local/core/api"
	"netlab.local/core/internal/access"
)

func TestBlueprintCreationsOwnIdentitiesAndPreserveNetworkDesign(t *testing.T) {
	raw := []byte(`{"networks":[{"id":"lan","name":"LAN","cidr":"10.1.0.0/24","dnsAssetId":"dns"}],"assets":[{"id":"dns","name":"DNS","templateId":"vm","resources":{"cpu":1,"memoryMiB":512,"diskGiB":8},"interfaces":[{"id":"nic","networkId":"lan","primary":true,"address":"10.1.0.2","mac":"02:00:00:00:00:02"}],"volumes":[{"id":"volume","mountPath":"/data","sizeGiB":1}]}],"routes":[{"networkId":"lan","destination":"10.2.0.0/24","nextHop":"10.1.0.3"}],"policies":[{"id":"policy","networkId":"lan","direction":"egress","action":"allow"}]}`)
	var versions [2]api.EnvironmentSpec
	var views [2]api.CanvasView
	identities := map[string]bool{"lan": true, "dns": true, "nic": true, "volume": true, "policy": true}
	for i := range versions {
		if err := json.Unmarshal(raw, &versions[i]); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(`{"positions":{"lan":{"x":10,"y":20},"dns":{"x":30,"y":40}},"collapsed":["lan"]}`), &views[i]); err != nil {
			t.Fatal(err)
		}
		instantiate(&versions[i], &views[i])
		spec, view := versions[i], views[i]
		network, asset := spec.Networks[0], spec.Assets[0]
		iface, volume, policy := asset.Interfaces[0], (*asset.Volumes)[0], (*spec.Policies)[0]
		for _, id := range []string{network.Id, asset.Id, iface.Id, volume.Id, policy.Id} {
			if id == "" || identities[id] {
				t.Fatalf("creation reused logical identity %q", id)
			}
			identities[id] = true
		}
		if *network.DnsAssetId != asset.Id || iface.NetworkId != network.Id || (*spec.Routes)[0].NetworkId != network.Id || policy.NetworkId != network.Id {
			t.Fatal("blueprint references were not mapped to the new environment")
		}
		if network.Cidr != "10.1.0.0/24" || iface.Address != "10.1.0.2" || iface.Mac != "02:00:00:00:00:02" || asset.TemplateId != "vm" {
			t.Fatal("saved network or template configuration changed")
		}
		if (*view.Positions)[network.Id] != (api.Point{X: 10, Y: 20}) || (*view.Positions)[asset.Id] != (api.Point{X: 30, Y: 40}) || (*view.Collapsed)[0] != network.Id {
			t.Fatal("canvas view no longer follows its objects")
		}
	}
}

func TestCreationUsesExactlyOneDesignSource(t *testing.T) {
	spec := api.EnvironmentSpec{Assets: []api.Asset{}, Networks: []api.Network{}}
	version := "version"
	service := Service{}
	for _, request := range []api.CreateEnvironment{{}, {Spec: &spec, BlueprintVersionId: &version}, {Spec: &spec, RecoveryPointId: &version}, {BlueprintVersionId: &version, RecoveryPointId: &version}} {
		if _, err := service.CreationSpec(context.Background(), access.Identity{}, request); err == nil {
			t.Fatal("ambiguous design source was accepted")
		}
	}
	resolved, err := service.CreationSpec(context.Background(), access.Identity{}, api.CreateEnvironment{Spec: &spec})
	if err != nil || !reflect.DeepEqual(resolved.Spec, spec) {
		t.Fatalf("inline design changed: %v", err)
	}
}
