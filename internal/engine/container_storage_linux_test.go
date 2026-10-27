//go:build linux

package engine

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/containerd"
	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	"github.com/containerd/containerd/services/introspection"
	"google.golang.org/genproto/googleapis/rpc/status"
)

type storageIntrospection struct {
	introspection.Service
	response *introspectionapi.PluginsResponse
	err      error
	filters  []string
}

func (s *storageIntrospection) Plugins(_ context.Context, filters []string) (*introspectionapi.PluginsResponse, error) {
	s.filters = filters
	return s.response, s.err
}

func TestContainerStorageUsesSnapshotterRoot(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name    string
		plugins []*introspectionapi.Plugin
		err     error
		message string
	}{
		{name: "exported root", plugins: []*introspectionapi.Plugin{{Exports: map[string]string{"root": root}}}},
		{name: "missing plugin", message: "snapshotter is unavailable"},
		{name: "missing root", plugins: []*introspectionapi.Plugin{{}}, message: "did not publish"},
		{name: "initialization failed", plugins: []*introspectionapi.Plugin{{InitErr: &status.Status{Message: "overlay is unavailable"}}}, message: "overlay is unavailable"},
		{name: "query failed", err: errors.New("introspection disconnected"), message: "introspection disconnected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &storageIntrospection{response: &introspectionapi.PluginsResponse{Plugins: test.plugins}, err: test.err}
			client, err := containerd.New("", containerd.WithServices(containerd.WithIntrospectionService(service)))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			containers := &Containers{client: client, data: t.TempDir()}
			info, err := containers.Storage(context.Background())
			if test.message != "" {
				if err == nil || !strings.Contains(err.Error(), test.message) || info.Path != "" {
					t.Fatalf("unavailable storage was replaced with a guessed path: %+v %v", info, err)
				}
				return
			}
			actual, statErr := os.Stat(root)
			if statErr != nil || !actual.IsDir() || err != nil || info.Path != root || info.Filesystem == "" || info.CapacityBytes <= 0 || info.AvailableBytes <= 0 {
				t.Fatalf("invalid actual snapshotter storage: %+v %v %v", info, err, statErr)
			}
			if !reflect.DeepEqual(service.filters, []string{"type==io.containerd.snapshotter.v1,id==overlayfs"}) {
				t.Fatalf("wrong snapshotter query: %v", service.filters)
			}
		})
	}
}
