//go:build linux

package engine

import (
	"testing"

	"github.com/containerd/containerd/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
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

func TestContainerMonitorStorageOwnership(t *testing.T) {
	spec := &oci.Spec{Mounts: []specs.Mount{{Destination: "/etc/resolv.conf", Source: "/var/lib/netlab/environments/env/instances/instance/resolv.conf"}}}
	if !managedContainer(spec, "/var/lib/netlab", "env", "instance") {
		t.Fatal("own instance was excluded")
	}
	if managedContainer(spec, "/var/lib/other-netlab", "env", "instance") {
		t.Fatal("monitor would take over another node's instance")
	}
}
