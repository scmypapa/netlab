//go:build linux

package engine

import (
	"context"
	"netlab.local/core/api"
)

func (e *Engine) AccessKey(environment string) (string, error)   { return e.access.Key(environment) }
func (e *Engine) SeedAccess(plan api.NodePlan, key string) error { return e.access.Seed(plan, key) }
func (e *Engine) RemoveLocalAccess(ctx context.Context, environment string) error {
	unlock := e.lock(environment)
	defer unlock()
	if err := e.access.Remove(ctx, environment); err != nil {
		return err
	}
	return e.gateway.Remove(ctx, environment, false)
}
