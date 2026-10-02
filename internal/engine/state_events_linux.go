//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	containerevents "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/typeurl/v2"
	"libvirt.org/go/libvirt"
	"netlab.local/core/api"
)

var libvirtEventOnce sync.Once
var libvirtEventError error

func libvirtEvents() error {
	libvirtEventOnce.Do(func() {
		libvirtEventError = libvirt.EventRegisterDefaultImpl()
		if libvirtEventError == nil {
			go func() {
				for {
					if err := libvirt.EventRunDefaultImpl(); err != nil {
						slog.Error("libvirt event loop stopped", "error", err)
						return
					}
				}
			}()
		}
	})
	return libvirtEventError
}

type runtimeChange struct {
	container, deleted bool
}

// Subscribe before inventory. Native callbacks only mark changed identities;
// slow subscribers cannot block libvirt's event thread or lose transitions.
func (e *Engine) Observe(ctx context.Context, publish func(api.NodeObservation) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = namespaces.WithNamespace(ctx, "netlab")
	var mu sync.Mutex
	dirty := map[string]runtimeChange{}
	failed := make(chan error, 1)
	mark := func(id string, change runtimeChange) {
		mu.Lock()
		dirty[id] = change
		mu.Unlock()
	}
	fail := func(err error) {
		select {
		case failed <- err:
		default:
		}
	}
	if e.container != nil {
		events, errs := e.container.client.Subscribe(ctx,
			`namespace=="netlab",topic=="/containers/create"`, `namespace=="netlab",topic=="/containers/update"`, `namespace=="netlab",topic=="/containers/delete"`,
			`namespace=="netlab",topic=="/tasks/create"`, `namespace=="netlab",topic=="/tasks/start"`, `namespace=="netlab",topic=="/tasks/exit"`,
			`namespace=="netlab",topic=="/tasks/delete"`, `namespace=="netlab",topic=="/tasks/paused"`, `namespace=="netlab",topic=="/tasks/resumed"`)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case err, open := <-errs:
					if !open {
						err = errors.New("containerd event stream closed")
					}
					fail(err)
					return
				case envelope, open := <-events:
					if !open {
						fail(errors.New("containerd event stream closed"))
						return
					}
					value, err := typeurl.UnmarshalAny(envelope.Event)
					if err != nil {
						fail(err)
						return
					}
					id, deleted := containerEvent(value)
					if id != "" {
						mark(id, runtimeChange{container: true, deleted: deleted})
					}
				}
			}
		}()
	}
	var vmConnection *libvirt.Connect
	if e.vm != nil {
		var err error
		vmConnection, err = libvirt.NewConnect(e.cfg.LibvirtURI)
		if err != nil {
			return err
		}
		defer vmConnection.Close()
		if err = vmConnection.RegisterCloseCallback(func(_ *libvirt.Connect, reason libvirt.ConnectCloseReason) {
			fail(fmt.Errorf("libvirt event connection closed: %d", reason))
		}); err != nil {
			return err
		}
		callback, err := vmConnection.DomainEventLifecycleRegister(nil, func(_ *libvirt.Connect, domain *libvirt.Domain, event *libvirt.DomainEventLifecycle) {
			id, err := domain.GetUUIDString()
			if err != nil {
				fail(err)
				return
			}
			mark(id, runtimeChange{deleted: event.Event == libvirt.DOMAIN_EVENT_UNDEFINED})
		})
		if err != nil {
			return err
		}
		defer vmConnection.DomainEventDeregister(callback)
	}
	snapshotAt := time.Now().UTC()
	snapshot, err := e.Inventory(ctx, "")
	if err != nil {
		return err
	}
	known := make(map[string]api.ExecutionResult, len(snapshot.Results))
	for i := range snapshot.Results {
		r := snapshot.Results[i]
		r.Execution = nil
		snapshot.Results[i] = r
		known[r.InstanceId] = r
	}
	if err = publish(api.NodeObservation{NodeId: e.cfg.ID, Snapshot: true, Results: snapshot.Results, ObservedAt: snapshotAt}); err != nil {
		return err
	}
	batch := time.NewTicker(100 * time.Millisecond)
	defer batch.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failed:
			return err
		case <-batch.C:
			mu.Lock()
			changes := dirty
			dirty = map[string]runtimeChange{}
			mu.Unlock()
			if len(changes) == 0 {
				continue
			}
			results := make([]api.ExecutionResult, 0, len(changes))
			for id, change := range changes {
				var r api.ExecutionResult
				if change.deleted {
					r = known[id]
					r.State, r.Error, r.ObservedAt = "absent", nil, time.Now().UTC()
					delete(known, id)
				} else if change.container {
					r, err = e.containerObservation(ctx, id)
				} else {
					r, err = e.vmObservation(vmConnection, id)
				}
				if err != nil {
					return err
				}
				if r.InstanceId == "" && known[id].InstanceId != "" {
					r = known[id]
					r.State, r.Error, r.ObservedAt = "absent", nil, time.Now().UTC()
					delete(known, id)
					change.deleted = true
				}
				if r.AssetId == "" || r.EnvironmentId == nil {
					continue
				}
				if !change.deleted {
					known[id] = r
				}
				results = append(results, r)
			}
			if len(results) > 0 {
				if err = publish(api.NodeObservation{NodeId: e.cfg.ID, Results: results, ObservedAt: time.Now().UTC()}); err != nil {
					return err
				}
			}
		case <-heartbeat.C:
			if err = publish(api.NodeObservation{NodeId: e.cfg.ID, Results: []api.ExecutionResult{}, ObservedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
	}
}

func containerEvent(value any) (string, bool) {
	switch event := value.(type) {
	case *containerevents.ContainerCreate:
		return event.ID, false
	case *containerevents.ContainerUpdate:
		return event.ID, false
	case *containerevents.ContainerDelete:
		return event.ID, true
	case *containerevents.TaskExit:
		if event.ID != event.ContainerID {
			return "", false // An exec process exiting does not stop its container.
		}
		return event.ContainerID, false
	case *containerevents.TaskDelete:
		if event.ID != "" && event.ID != event.ContainerID {
			return "", false
		}
		return event.ContainerID, false
	case interface{ GetContainerID() string }:
		return event.GetContainerID(), false
	default:
		return "", false
	}
}

func (e *Engine) containerObservation(ctx context.Context, id string) (api.ExecutionResult, error) {
	container, err := e.container.client.LoadContainer(ctx, id)
	if errdefs.IsNotFound(err) {
		return api.ExecutionResult{}, nil
	}
	if err != nil {
		return api.ExecutionResult{}, err
	}
	labels, spec, err := containerMetadata(ctx, container)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	if labels[environmentLabel] == "" || labels[assetLabel] == "" {
		return api.ExecutionResult{}, nil
	}
	if !managedContainer(spec, e.cfg.DataDir, labels[environmentLabel], id) {
		return api.ExecutionResult{}, nil
	}
	state, err := containerState(ctx, container)
	r := api.ExecutionResult{EnvironmentId: ptr(labels[environmentLabel]), AssetId: labels[assetLabel], InstanceId: id, State: state, ObservedAt: time.Now().UTC()}
	if message := labels[restartErrorLabel]; message != "" {
		err = errors.Join(err, errors.New(message))
	}
	if err != nil {
		r.Error = ptr(err.Error())
	}
	return r, nil
}

func (e *Engine) vmObservation(connection *libvirt.Connect, id string) (api.ExecutionResult, error) {
	domain, err := connection.LookupDomainByUUIDString(id)
	if noDomain(err) {
		return api.ExecutionResult{}, nil
	}
	if err != nil {
		return api.ExecutionResult{}, err
	}
	defer domain.Free()
	owner, err := e.vm.owned(domain, "", "")
	if err != nil {
		var failure libvirt.Error
		if errors.As(err, &failure) && !noDomain(err) {
			return api.ExecutionResult{}, err
		}
		return api.ExecutionResult{}, nil // Other libvirt domains are outside this node's managed objects.
	}
	state, err := vmState(domain)
	if noDomain(err) {
		return api.ExecutionResult{}, nil
	}
	r := api.ExecutionResult{EnvironmentId: ptr(owner.Environment), AssetId: owner.Asset, InstanceId: owner.Instance, State: state, ObservedAt: time.Now().UTC()}
	if err != nil {
		r.Error = ptr(err.Error())
	}
	return r, nil
}
