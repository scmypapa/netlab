//go:build linux

package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netlab.local/core/api"
)

func TestParallelTemplatePreparationAndTrim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e := &Engine{cfg: Config{DataDir: t.TempDir()}, locks: map[string]*objectLock{}, slots: make(chan struct{}, 2)}
	template := api.Template{Id: "system", Kind: api.Vm, Version: 1}
	first, second := e.lock("one/guest"), e.lock("two/guest")
	defer func() {
		if first != nil {
			first()
		}
		if second != nil {
			second()
		}
	}()
	finished := make(chan api.NodeResult, 2)
	for _, env := range []string{"one", "two"} {
		go func() {
			finished <- e.Execute(ctx, api.NodePlan{EnvironmentId: env, Phase: api.NodePlanPhasePrepare, Assets: []api.AssetExecution{{Template: template, Asset: api.Asset{Id: "guest"}}}})
		}()
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(e.slots) != 2 {
		select {
		case <-ctx.Done():
			t.Fatal("same-template preparation did not enter both instance slots")
		case <-ticker.C:
		}
	}
	trimmed := make(chan error, 1)
	go func() {
		_, err := e.TrimTemplateCache(ctx, api.TemplateCacheRequest{Template: template})
		trimmed <- err
	}()
	first()
	first = nil
	select {
	case result := <-finished:
		// No compute runtime is installed: this test exercises the real plan/trim locking path.
		if len(result.Results) != 1 || result.Results[0].Error == nil || !strings.Contains(*result.Results[0].Error, "runtime not configured") {
			t.Fatalf("unexpected execution result: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("first preparation did not finish")
	}
	select {
	case err := <-trimmed:
		t.Fatalf("cache trim passed a preparation still reading the template: %v", err)
	default:
	}
	second()
	second = nil
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("second preparation did not finish")
	}
	select {
	case err := <-trimmed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("cache trim did not finish after both preparations released the template")
	}
	if len(e.locks) != 0 || len(e.slots) != 0 {
		t.Fatal("completed preparations retained locks or slots")
	}
}

func TestColdTemplateSourceDoesNotOccupyComputeSlot(t *testing.T) {
	for _, kind := range []api.TemplateKind{api.Container, api.Vm} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			origin := "origin"
			template := api.Template{Id: "cold", Kind: kind, Version: 1, ArtifactNodeId: &origin}
			files := map[string][]byte{"image.tar": []byte("image archive")}
			if kind == api.Vm {
				template.Disks = &[]api.TemplateDisk{{Id: "boot"}}
				files = map[string][]byte{"disk-0.qcow2": []byte("virtual disk")}
			}
			manifest, err := json.Marshal(template)
			if err != nil {
				t.Fatal(err)
			}
			files["template.json"] = manifest
			source := &Engine{cfg: Config{ID: origin, DataDir: t.TempDir()}, locks: map[string]*objectLock{}}
			directory := templateDirectory(source.cfg.DataDir, template.Id, template.Version)
			if err := os.MkdirAll(directory, 0711); err != nil {
				t.Fatal(err)
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(directory, name), data, 0640); err != nil {
					t.Fatal(err)
				}
			}
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				reader, _, err := source.OpenTemplateArtifact(r.Context(), template.Id, template.Version, false)
				if err != nil {
					t.Error(err)
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				defer reader.Close()
				io.Copy(w, reader)
			}))
			defer server.Close()
			e := &Engine{cfg: Config{ID: "target", DataDir: t.TempDir(), ArtifactHTTP: server.Client()}, locks: map[string]*objectLock{}, slots: make(chan struct{}, 1), ioSlots: make(chan struct{}, 1)}
			endpoints := map[string]string{origin: server.URL}
			cold := make(chan api.NodeResult, 1)
			go func() {
				cold <- e.Execute(ctx, api.NodePlan{EnvironmentId: "loading", Phase: api.NodePlanPhasePrepare, ArtifactEndpoints: &endpoints, Assets: []api.AssetExecution{{Template: template, Asset: api.Asset{Id: "guest"}}}})
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("cold source request did not start")
			}
			if len(e.slots) != 0 || len(e.ioSlots) != 1 {
				t.Fatal("cold source preparation did not isolate I/O from compute slots")
			}
			control := make(chan api.NodeResult, 1)
			go func() {
				control <- e.Execute(ctx, api.NodePlan{EnvironmentId: "other", Phase: api.NodePlanPhaseInspect, Assets: []api.AssetExecution{{Template: api.Template{Id: "ready", Kind: kind}, Asset: api.Asset{Id: "other"}}}})
			}()
			select {
			case result := <-control:
				if len(result.Results) != 1 {
					t.Fatal("unrelated instance control did not execute")
				}
			case <-ctx.Done():
				t.Fatal("unrelated instance control waited for cold template transfer")
			}
			close(release)
			select {
			case <-cold:
			case <-ctx.Done():
				t.Fatal("source preparation did not finish")
			}
			if len(e.slots) != 0 || len(e.ioSlots) != 0 {
				t.Fatal("completed source preparation retained execution slots")
			}
			if _, err := os.Stat(filepath.Join(templateDirectory(e.cfg.DataDir, template.Id, template.Version), "template.json")); err != nil {
				t.Fatal("source preparation did not commit its artifact:", err)
			}
		})
	}
}
