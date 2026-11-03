//go:build linux

package engine

import (
	"context"
	"golang.org/x/sync/semaphore"
	"math"
	"sync"
)

type objectLocks struct {
	mu      sync.Mutex
	entries map[string]*objectLock
}
type objectLock struct {
	mu   *semaphore.Weighted
	refs int
}

func (locks *objectLocks) acquire(ctx context.Context, id string, shared bool) (func(), error) {
	locks.mu.Lock()
	if locks.entries == nil {
		locks.entries = make(map[string]*objectLock)
	}
	l := locks.entries[id]
	if l == nil {
		l = &objectLock{mu: semaphore.NewWeighted(math.MaxInt64)}
		locks.entries[id] = l
	}
	l.refs++
	locks.mu.Unlock()
	weight := int64(math.MaxInt64)
	if shared {
		weight = 1
	}
	release := func() {
		locks.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(locks.entries, id)
		}
		locks.mu.Unlock()
	}
	if err := l.mu.Acquire(ctx, weight); err != nil {
		release()
		return nil, err
	}
	return func() { l.mu.Release(weight); release() }, nil
}
