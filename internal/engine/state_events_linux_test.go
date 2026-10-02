//go:build linux

package engine

import (
	"context"
	"testing"

	"github.com/containerd/containerd"
	containerevents "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/typeurl/v2"
)

type deletedContainerStore struct {
	containers.Store
	record containers.Container
	reads  int
}

func (s *deletedContainerStore) Get(context.Context, string) (containers.Container, error) {
	s.reads++
	if s.reads > 1 {
		return containers.Container{}, errdefs.ErrNotFound
	}
	return s.record, nil
}

func TestContainerMetadataSurvivesDeletionAfterLoad(t *testing.T) {
	spec, err := typeurl.MarshalAny(&oci.Spec{Version: "1.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	store := &deletedContainerStore{record: containers.Container{ID: "instance", Labels: map[string]string{assetLabel: "asset"}, Spec: spec}}
	client, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(store)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	container, err := client.LoadContainer(context.Background(), "instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = container.Labels(context.Background()); !errdefs.IsNotFound(err) {
		t.Fatalf("container should be deleted after load: %v", err)
	}
	labels, actual, err := containerMetadata(context.Background(), container)
	if err != nil || labels[assetLabel] != "asset" || actual.Version != "1.0.2" || store.reads != 2 {
		t.Fatalf("loaded metadata was reread after deletion: labels=%v spec=%+v reads=%d error=%v", labels, actual, store.reads, err)
	}
}

func TestContainerEventsDistinguishTasksAndExecProcesses(t *testing.T) {
	for _, item := range []struct {
		value   any
		id      string
		deleted bool
	}{
		{&containerevents.TaskExit{ContainerID: "instance", ID: "instance"}, "instance", false},
		{&containerevents.TaskExit{ContainerID: "instance", ID: "terminal-command"}, "", false},
		{&containerevents.TaskDelete{ContainerID: "instance", ID: "terminal-command"}, "", false},
		{&containerevents.TaskDelete{ContainerID: "instance"}, "instance", false},
		{&containerevents.ContainerDelete{ID: "instance"}, "instance", true},
		{&containerevents.ContainerUpdate{ID: "instance"}, "instance", false},
		{&containerevents.TaskPaused{ContainerID: "instance"}, "instance", false},
		{&containerevents.TaskResumed{ContainerID: "instance"}, "instance", false},
	} {
		id, deleted := containerEvent(item.value)
		if id != item.id || deleted != item.deleted {
			t.Fatalf("%T: id=%s deleted=%v", item.value, id, deleted)
		}
	}
}
