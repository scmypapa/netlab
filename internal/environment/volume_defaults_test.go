package environment

import (
	"reflect"
	"testing"

	"netlab.local/core/api"
)

func TestVolumeDefaultsBeforeScheduling(t *testing.T) {
	resources := api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}
	defaults := []api.Volume{{Id: "custom", MountPath: "/custom", SizeGiB: 3}}
	for _, test := range []struct {
		name  string
		kind  api.TemplateKind
		input *[]api.Volume
		want  []api.Volume
	}{
		{"image and template defaults", api.Container, nil, []api.Volume{defaults[0], {Id: "image-volume-0", MountPath: "/cache", SizeGiB: 1}, {Id: "image-volume-1", MountPath: "/data", SizeGiB: 1}}},
		{"override one image path", api.Container, &[]api.Volume{{Id: "selected", MountPath: "/data", SizeGiB: 8}}, []api.Volume{{Id: "selected", MountPath: "/data", SizeGiB: 8}, {Id: "image-volume-0", MountPath: "/cache", SizeGiB: 1}}},
		{"omit optional template volume", api.Container, &[]api.Volume{}, []api.Volume{{Id: "image-volume-0", MountPath: "/cache", SizeGiB: 1}, {Id: "image-volume-1", MountPath: "/data", SizeGiB: 1}}},
		{"remove VM data disks", api.Vm, &[]api.Volume{}, []api.Volume{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			template := api.Template{Id: "template", Kind: test.kind, Resources: resources, Volumes: &defaults, ImageVolumes: &[]string{"/cache", "/data"}}
			asset := api.Asset{Id: "asset", TemplateId: template.Id, Resources: resources, Volumes: test.input}
			spec, err := Normalize(api.EnvironmentSpec{Assets: []api.Asset{asset}}, map[string]api.Template{template.Id: template})
			if err != nil || spec.Assets[0].Volumes == nil || !reflect.DeepEqual(*spec.Assets[0].Volumes, test.want) {
				t.Fatalf("resolved volumes=%+v, error=%v", spec.Assets[0].Volumes, err)
			}
		})
	}
}
