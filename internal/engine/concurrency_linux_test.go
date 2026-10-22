//go:build linux

package engine

import (
	"context"
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
