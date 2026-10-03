//go:build linux

package engine

import (
	"testing"

	"netlab.local/core/api"
)

func TestContainerRestartPolicy(t *testing.T) {
	for _, test := range []struct {
		policy *api.RestartPolicy
		code   uint32
		want   bool
	}{
		{nil, 1, false}, {ptr(api.Never), 1, false},
		{ptr(api.OnFailure), 0, false}, {ptr(api.OnFailure), 1, true},
		{ptr(api.Always), 0, true}, {ptr(api.Always), 137, true},
	} {
		if actual := restartAfterExit(test.policy, test.code); actual != test.want {
			t.Fatalf("policy=%v code=%d restart=%v", test.policy, test.code, actual)
		}
	}
}

func TestContainerMonitorOwnership(t *testing.T) {
	labels := map[string]string{nodeLabel: "node", environmentLabel: "env", assetLabel: "asset"}
	if !managedContainer(labels, "node") {
		t.Fatal("own instance was excluded")
	}
	if managedContainer(labels, "other-node") {
		t.Fatal("monitor would take over another node's instance")
	}
}
