//go:build linux

package engine

import (
	"testing"

	containerevents "github.com/containerd/containerd/api/events"
)

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
