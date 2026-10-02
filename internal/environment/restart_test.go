package environment

import (
	"testing"

	"netlab.local/core/api"
)

func TestContainerRestartPolicyChangeIsLive(t *testing.T) {
	before := api.Asset{Id: "web", Name: "Web", TemplateId: "web", RestartPolicy: restartPolicy(api.Never)}
	after := before
	after.RestartPolicy = restartPolicy(api.Always)
	if RequiresStop(api.Container, before, after) {
		t.Fatal("restart policy change would unnecessarily stop a running container")
	}
}

func TestRestartPolicyRequiresContainer(t *testing.T) {
	for _, test := range []struct {
		kind      api.TemplateKind
		policy    api.RestartPolicy
		wantValid bool
	}{
		{api.Container, api.Always, true}, {api.Container, api.OnFailure, true},
		{api.Container, "unknown", false}, {api.Vm, api.Always, false},
	} {
		asset := api.Asset{Id: "web", Name: "Web", TemplateId: "template", RestartPolicy: restartPolicy(test.policy), Resources: api.Resources{Cpu: 1, MemoryMiB: 128, DiskGiB: 1}}
		_, err := Normalize(api.EnvironmentSpec{Assets: []api.Asset{asset}}, map[string]api.Template{"template": {Id: "template", Kind: test.kind, Resources: asset.Resources}})
		if (err == nil) != test.wantValid {
			t.Fatalf("kind=%s policy=%s error=%v", test.kind, test.policy, err)
		}
	}
}

func restartPolicy(value api.RestartPolicy) *api.RestartPolicy { return &value }
