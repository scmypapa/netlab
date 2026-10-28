//go:build linux

package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"netlab.local/core/api"
)

func TestRealContainerImportDoesNotOccupyCompute(t *testing.T) {
	if os.Getenv("NETLAB_REAL_CONTAINER") == "" {
		t.Skip("set NETLAB_REAL_CONTAINER to exercise actual containerd import and unpack")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := os.MkdirTemp("/tmp", "netlab-cold-container-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(data)
	archive := nonRootContainerArchive(t, ctx, data)
	c, err := NewContainers(ctx, "/run/containerd/containerd.sock", data, uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	e := &Engine{cfg: Config{ID: c.node, DataDir: data}, container: c, locks: map[string]*objectLock{}, slots: make(chan struct{}, 1), ioSlots: make(chan struct{}, 1)}
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		file, err := os.Open(archive)
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		io.Copy(w, file)
	}))
	defer source.Close()
	defer unblock()
	resources := api.Resources{Cpu: 1, MemoryMiB: 64, DiskGiB: 1}
	template := api.Template{Id: uuid.NewString(), Kind: api.Container, Version: 1, Source: source.URL, Resources: resources}
	warmTemplate := template
	warmTemplate.Id, warmTemplate.Source = uuid.NewString(), archive
	warmTemplate, err = e.PrepareTemplate(ctx, api.NodeTemplatePreparation{Template: warmTemplate})
	if err != nil || warmTemplate.ArtifactNodeId == nil {
		t.Fatalf("prepare the warm image through the production template path: %+v %v", warmTemplate, err)
	}
	a := api.AssetExecution{InstanceId: uuid.NewString(), Template: template, Asset: api.Asset{Id: "server", Name: "Server", Resources: resources}, Interfaces: []api.ResolvedInterface{}}
	plan := api.NodePlan{EnvironmentId: uuid.NewString(), OperationId: uuid.NewString(), Phase: api.NodePlanPhasePrepare, Assets: []api.AssetExecution{a}}
	warm := plan
	warm.EnvironmentId = uuid.NewString()
	warm.Assets = []api.AssetExecution{a}
	warm.Assets[0].InstanceId, warm.Assets[0].Template = uuid.NewString(), warmTemplate
	done := make(chan api.NodeResult, 1)
	go func() { done <- e.Execute(ctx, plan) }()
	completed := false
	defer func() {
		unblock()
		cancel()
		if !completed {
			<-done
		}
		cleanup, finish := context.WithTimeout(context.Background(), 10*time.Second)
		defer finish()
		for _, input := range []api.NodePlan{plan, warm} {
			input.Phase = api.NodePlanPhaseDestroy
			result := e.Execute(cleanup, input)
			if result.Error != nil {
				t.Error(*result.Error)
			}
			for _, result := range result.Results {
				if result.Error != nil {
					t.Error(*result.Error)
				}
			}
		}
		for _, id := range []string{template.Id, warmTemplate.Id} {
			if err := e.RemoveTemplate(cleanup, id, nil); err != nil {
				t.Error(err)
			}
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("actual image import did not open its source", ctx.Err())
	}
	if len(e.slots) != 0 || len(e.ioSlots) != 1 {
		t.Fatalf("cold image import occupied compute: compute=%d io=%d", len(e.slots), len(e.ioSlots))
	}
	control, finish := context.WithTimeout(ctx, time.Second)
	other := api.NodePlan{EnvironmentId: uuid.NewString(), Phase: api.NodePlanPhaseInspect, Assets: []api.AssetExecution{{InstanceId: uuid.NewString(), Asset: a.Asset, Template: template}}}
	result := e.Execute(control, other)
	finish()
	if result.Error != nil || len(result.Results) != 1 || result.Results[0].State != "absent" {
		t.Fatalf("another environment waited behind image import: %+v", result)
	}
	control, finish = context.WithTimeout(ctx, time.Second)
	result = e.Execute(control, warm)
	finish()
	if result.Error != nil || len(result.Results) != 1 || result.Results[0].State != "prepared" || result.Results[0].Error != nil {
		t.Fatalf("an already unpacked image waited behind cold image I/O: %+v", result)
	}
	unblock()
	select {
	case result = <-done:
		completed = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if result.Error != nil || len(result.Results) != 1 || result.Results[0].State != "prepared" || result.Results[0].Error != nil {
		t.Fatalf("real image import and instance preparation failed: %+v", result)
	}
	if len(e.slots) != 0 || len(e.ioSlots) != 0 {
		t.Fatal("image preparation retained execution slots")
	}
}
