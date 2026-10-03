//go:build linux

package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/namespaces"
	"golang.org/x/sys/unix"
	"netlab.local/core/api"
	"netlab.local/core/internal/network"
)

type Config struct {
	ID, Name, DataDir, ContainerdSocket, LibvirtURI, OVNEndpoint, OVSEndpoint, Bridge string
	ProviderCIDR                                                                      string
	AdvertiseAddress                                                                  string
	ArtifactHTTP                                                                      *http.Client
}
type Engine struct {
	cfg       Config
	container *Containers
	vm        *VirtualMachines
	ovn       *network.OVN
	ovs       *network.OVS
	gateway   *network.Gateway
	access    *network.Access
	external  *network.External
	slots     chan struct{}
	ioSlots   chan struct{}
	mu        sync.Mutex
	locks     map[string]*objectLock
}
type objectLock struct {
	mu   sync.Mutex
	refs int
}

func New(ctx context.Context, cfg Config) (*Engine, error) {
	if cfg.ProviderCIDR == "" {
		cfg.ProviderCIDR = "100.127.0.0/16"
	}
	if err := os.MkdirAll(cfg.DataDir, 0711); err != nil {
		return nil, err
	}
	concurrency := max(1, runtime.NumCPU()/2)
	e := &Engine{cfg: cfg, slots: make(chan struct{}, concurrency), ioSlots: make(chan struct{}, concurrency), locks: make(map[string]*objectLock)}
	var err error
	if e.ovs, err = network.NewOVS(ctx, cfg.OVSEndpoint, cfg.Bridge); err != nil {
		return nil, err
	}
	if e.ovn, err = network.NewOVN(ctx, cfg.OVNEndpoint); err != nil {
		e.Close()
		return nil, err
	}
	if e.external, err = network.NewExternal(ctx, e.ovs, cfg.DataDir, cfg.ID); err != nil {
		e.Close()
		return nil, err
	}
	if e.gateway, err = network.NewGateway(ctx, cfg.DataDir, cfg.ID, cfg.ProviderCIDR, e.ovs, e.ovn); err != nil {
		e.Close()
		return nil, err
	}
	if e.access, err = network.NewAccess(ctx, cfg.DataDir, e.ovs, e.ovn); err != nil {
		e.Close()
		return nil, err
	}
	if cfg.ContainerdSocket != "" {
		if e.container, err = NewContainers(ctx, cfg.ContainerdSocket, cfg.DataDir, cfg.ID, e.ovs); err != nil {
			e.Close()
			return nil, err
		}
	}
	if cfg.LibvirtURI != "" {
		if e.vm, err = NewVirtualMachines(cfg.LibvirtURI, cfg.DataDir, cfg.Bridge); err != nil {
			e.Close()
			return nil, err
		}
	}
	if e.container == nil && e.vm == nil {
		e.Close()
		return nil, errors.New("no compute runtime configured")
	}
	if e.container != nil {
		if err = e.superviseContainers(); err != nil {
			e.Close()
			return nil, err
		}
	}
	return e, nil
}
func (e *Engine) Close() {
	if e.gateway != nil {
		e.gateway.Close()
	}
	if e.container != nil {
		e.container.Close()
	}
	if e.vm != nil {
		e.vm.Close()
	}
	if e.ovn != nil {
		e.ovn.Close()
	}
	if e.ovs != nil {
		e.ovs.Close()
	}
}
func (e *Engine) lock(id string) func() {
	e.mu.Lock()
	l := e.locks[id]
	if l == nil {
		l = &objectLock{}
		e.locks[id] = l
	}
	l.refs++
	e.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		e.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(e.locks, id)
		}
		e.mu.Unlock()
	}
}
func (e *Engine) Info() (api.NodeInfo, error) {
	var mem unix.Sysinfo_t
	if err := unix.Sysinfo(&mem); err != nil {
		return api.NodeInfo{}, err
	}
	var disk unix.Statfs_t
	if err := unix.Statfs(e.cfg.DataDir, &disk); err != nil {
		return api.NodeInfo{}, err
	}
	caps := []string{"network"}
	if e.container != nil {
		caps = append(caps, "container")
	}
	if e.vm != nil {
		caps = append(caps, "vm")
	}
	info := api.NodeInfo{Id: e.cfg.ID, Name: e.cfg.Name, Slots: cap(e.slots), Capabilities: caps, Capacity: api.Resources{Cpu: runtime.NumCPU(), MemoryMiB: int64(mem.Totalram) * int64(mem.Unit) / (1 << 20), DiskGiB: int64(disk.Blocks) * int64(disk.Bsize) / (1 << 30)}}
	storage, err := StorageInfo(e.cfg.DataDir)
	if err != nil {
		return api.NodeInfo{}, err
	}
	info.Storage = &storage
	info.ServiceNetwork = ptr(e.gateway.Network())
	address, err := network.AccessAddress(e.cfg.AdvertiseAddress)
	if err != nil {
		return api.NodeInfo{}, err
	}
	info.AccessAddress = &address
	info.NetworkChassis = ptr(e.ovn.Chassis())
	interfaces, err := e.external.Interfaces(context.Background())
	if err != nil {
		return api.NodeInfo{}, err
	}
	info.ExternalInterfaces = &interfaces
	if e.vm != nil {
		info.VmHardware = &e.vm.hardware
	}
	return info, nil
}
func (e *Engine) Execute(ctx context.Context, plan api.NodePlan) api.NodeResult {
	unlocked := e.lock(plan.EnvironmentId)
	defer unlocked()
	result := api.NodeResult{Results: []api.ExecutionResult{}}
	if plan.Gateway != nil && (plan.Phase == api.NodePlanPhaseNetwork || plan.Phase == api.NodePlanPhaseServices) && plan.Gateway.NodeId != e.cfg.ID {
		result.Error = ptr("service gateway plan was sent to another node")
		return result
	}
	switch plan.Phase {
	case api.NodePlanPhaseExternalAttachments:
		var attachments []api.ExternalAttachment
		if plan.Attachments != nil {
			attachments = *plan.Attachments
		}
		if err := e.external.Apply(ctx, plan.EnvironmentId, attachments); err != nil {
			result.Error = ptr(err.Error())
		}
		return result
	case api.NodePlanPhaseNetwork:
		if err := e.ovn.Apply(ctx, plan); err != nil {
			result.Error = ptr(err.Error())
			return result
		}
		if err := e.access.Prepare(ctx, plan); err != nil {
			result.Error = ptr(err.Error())
		}
		return result
	case api.NodePlanPhaseRemoveNetwork:
		if err := e.access.Remove(ctx, plan.EnvironmentId); err != nil {
			result.Error = ptr(err.Error())
			return result
		}
		if err := e.gateway.Remove(ctx, plan.EnvironmentId); err != nil {
			result.Error = ptr(err.Error())
		}
		return result
	case api.NodePlanPhaseVpn:
		value, err := e.access.Apply(ctx, plan)
		if err != nil {
			result.Error = ptr(err.Error())
		} else {
			result.Vpn = &value
		}
		return result
	case api.NodePlanPhaseServices:
		services, err := e.gateway.Apply(ctx, plan)
		if err != nil {
			result.Error = ptr(err.Error())
		} else {
			result.Services = &services
		}
		return result
	}
	result.Results = make([]api.ExecutionResult, len(plan.Assets))
	policies := policiesByNetwork(plan.Spec)
	var wg sync.WaitGroup
	for i, asset := range plan.Assets {
		wg.Add(1)
		go func(i int, a api.AssetExecution) {
			defer wg.Done()
			select {
			case e.slots <- struct{}{}:
			case <-ctx.Done():
				result.Results[i] = executionResult(a, "unknown", ctx.Err())
				return
			}
			defer func() { <-e.slots }()
			unlock := e.lock(plan.EnvironmentId + "/" + a.Asset.Id)
			defer unlock()
			var state string
			var err error
			phase := plan.Phase
			if (phase == api.NodePlanPhasePrepare || phase == api.NodePlanPhasePrepareRecovery) && a.Template.ArtifactNodeId != nil {
				endpoint := ""
				if plan.ArtifactEndpoints != nil {
					endpoint = (*plan.ArtifactEndpoints)[*a.Template.ArtifactNodeId]
				}
				if err = e.fetchTemplateArtifact(ctx, a.Template, endpoint); err != nil {
					result.Results[i] = executionResult(a, "absent", err)
					return
				}
			}
			if plan.Phase == api.NodePlanPhasePrepareRecovery || plan.Phase == api.NodePlanPhaseApplyRecovery || plan.Phase == api.NodePlanPhaseRollbackRecovery || plan.Phase == api.NodePlanPhaseCleanupRecovery {
				switch plan.Phase {
				case api.NodePlanPhasePrepareRecovery:
					err = e.prepareRecovery(ctx, plan, a)
					state = "prepared"
				case api.NodePlanPhaseApplyRecovery:
					if a.Template.Kind == api.Container {
						state, err = e.container.applyRecovery(ctx, plan.EnvironmentId, a)
					} else {
						state, err = e.vm.applyRecovery(ctx, plan.EnvironmentId, a)
					}
				case api.NodePlanPhaseRollbackRecovery:
					state, err = e.rollbackRecovery(ctx, plan.EnvironmentId, plan.OperationId, a)
				case api.NodePlanPhaseCleanupRecovery:
					state, err = "cleaned", e.cleanupRecovery(ctx, plan.EnvironmentId, plan.OperationId, a)
				}
				result.Results[i] = executionResult(a, state, err)
				return
			}
			if plan.Phase == api.NodePlanPhaseCaptureRecovery || plan.Phase == api.NodePlanPhaseDeleteRecovery {
				if plan.RecoveryPointId == nil {
					result.Results[i] = executionResult(a, "unknown", errors.New("missing recovery point identity"))
					return
				}
				if plan.Phase == api.NodePlanPhaseDeleteRecovery {
					result.Results[i] = executionResult(a, "deleted", e.deleteRecovery(plan.EnvironmentId, *plan.RecoveryPointId, a))
				} else {
					quiesce := plan.CaptureStates != nil && (*plan.CaptureStates)[a.Asset.Id] == "running" && !plan.IncludeMemory
					capture, captureErr := e.captureRecovery(ctx, plan.EnvironmentId, *plan.RecoveryPointId, a, plan.IncludeMemory, quiesce)
					r := executionResult(a, "captured", captureErr)
					if captureErr == nil {
						r.Recovery = &capture
					}
					result.Results[i] = r
				}
				return
			}
			if phase == api.NodePlanPhasePolicies {
				phase = api.NodePlanPhaseInspect
			}
			switch a.Template.Kind {
			case api.Container:
				if e.container == nil {
					err = errors.New("container runtime not configured")
				} else {
					state, err = e.container.Execute(ctx, plan.EnvironmentId, phase, a)
				}
			case api.Vm:
				if e.vm == nil {
					err = errors.New("virtual machine runtime not configured")
				} else {
					if phase == api.NodePlanPhaseStart && plan.RecoveryPointId != nil {
						if err = e.vm.finishRecoveryFS(plan.EnvironmentId, *plan.RecoveryPointId, a, false); err != nil {
							result.Results[i] = executionResult(a, "unknown", err)
							return
						}
					}
					state, err = e.vm.Execute(ctx, plan.EnvironmentId, phase, a)
				}
			default:
				err = fmt.Errorf("invalid compute kind %s", a.Template.Kind)
			}
			if err == nil && (plan.Phase == api.NodePlanPhasePolicies || (plan.Phase == api.NodePlanPhaseStart && state == "running")) {
				err = e.shape(ctx, plan.EnvironmentId, a, policies)
				if err == nil && a.Template.Kind == api.Container {
					err = e.container.savePolicies(ctx, a.InstanceId, plan.Spec.Policies)
				}
			}
			r := executionResult(a, state, err)
			if plan.Phase == api.NodePlanPhaseUpdate || plan.Phase == api.NodePlanPhaseInspect {
				var observed *api.AssetExecution
				var observeErr error
				if a.Template.Kind == api.Container && e.container != nil {
					container, loadErr := e.container.client.LoadContainer(ctx, a.InstanceId)
					if loadErr != nil {
						observeErr = loadErr
					} else {
						observed, observeErr = e.container.observedExecution(ctx, container)
						if observed != nil && r.State == "unknown" {
							var stateErr error
							r.State, stateErr = containerState(ctx, container)
							observeErr = errors.Join(observeErr, stateErr)
						}
					}
				} else if a.Template.Kind == api.Vm && e.vm != nil {
					domain, loadErr := e.vm.conn.LookupDomainByUUIDString(a.InstanceId)
					if loadErr != nil {
						observeErr = loadErr
					} else {
						observed, observeErr = e.vm.observedExecution(domain)
						if observed != nil && r.State == "unknown" {
							var stateErr error
							r.State, stateErr = vmState(domain)
							observeErr = errors.Join(observeErr, stateErr)
						}
						domain.Free()
					}
				}
				r.Execution = observed
				if err = errors.Join(err, observeErr); err != nil {
					r.Error = ptr(err.Error())
				}
			}
			result.Results[i] = r
		}(i, asset)
	}
	wg.Wait()
	return result
}
func (e *Engine) PrepareTemplate(ctx context.Context, request api.NodeTemplatePreparation) (api.Template, error) {
	t := request.Template
	if request.Capture != nil {
		unlock := e.lock(request.Capture.EnvironmentId)
		defer unlock()
	}
	unlock := e.lock("template:" + t.Id)
	defer unlock()
	select {
	case e.slots <- struct{}{}:
	case <-ctx.Done():
		return t, ctx.Err()
	}
	defer func() { <-e.slots }()
	var err error
	if t.ArtifactNodeId != nil {
		endpoint := ""
		if request.ArtifactEndpoint != nil {
			endpoint = *request.ArtifactEndpoint
		}
		if err = e.fetchTemplateArtifact(ctx, t, endpoint); err != nil {
			return t, err
		}
	}
	if t.ArtifactNodeId == nil {
		t.ArtifactNodeId = &e.cfg.ID
	}
	switch t.Kind {
	case api.Container:
		if e.container == nil {
			return t, errors.New("container runtime not configured")
		}
		t, err = e.container.prepareTemplate(namespaces.WithNamespace(ctx, "netlab"), t, request.Registry)
	case api.Vm:
		if e.vm == nil {
			return t, errors.New("virtual machine runtime not configured")
		}
		t, err = e.vm.prepareTemplate(ctx, t, request.Capture)
	default:
		err = fmt.Errorf("invalid template kind %s", t.Kind)
	}
	if err != nil {
		t.State = ptr(api.TemplateStateFailed)
		t.Error = ptr(err.Error())
		return t, err
	}
	directory := importDirectory(e.cfg.DataDir, t.Id, t.Version)
	if relative, uploaded := strings.CutPrefix(t.Source, directory+string(filepath.Separator)); uploaded {
		source, pathErr := artifactPath(directory, strings.SplitN(relative, string(filepath.Separator), 2)[0])
		if pathErr != nil {
			return t, pathErr
		}
		if err = os.RemoveAll(source); err != nil {
			return t, err
		}
	}
	t.State = ptr(api.TemplateStateReady)
	t.Error = nil
	return t, nil
}
func (e *Engine) Inventory(ctx context.Context, environmentID string) (api.NodeResult, error) {
	result := api.NodeResult{Results: []api.ExecutionResult{}}
	if e.container != nil {
		items, err := e.container.Inventory(ctx, environmentID)
		if err != nil {
			return result, err
		}
		result.Results = append(result.Results, items...)
	}
	if e.vm != nil {
		items, err := e.vm.Inventory(environmentID)
		if err != nil {
			return result, err
		}
		result.Results = append(result.Results, items...)
	}
	return result, nil
}
func executionResult(a api.AssetExecution, state string, err error) api.ExecutionResult {
	if state == "" {
		state = "unknown"
	}
	r := api.ExecutionResult{AssetId: a.Asset.Id, InstanceId: a.InstanceId, State: state, ObservedAt: time.Now().UTC()}
	if err != nil {
		r.Error = ptr(err.Error())
	}
	return r
}
func ptr[T any](v T) *T { return &v }
func instanceDir(data, env, instance string) string {
	return filepath.Join(data, "environments", env, "instances", instance)
}
