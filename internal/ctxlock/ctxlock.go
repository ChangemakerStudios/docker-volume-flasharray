// Package ctxlock is a mutex whose Lock gives up when a context ends.
package ctxlock

import (
	"context"
	"sync"
)

// Mutex is a mutual exclusion lock; the zero value is unlocked.
type Mutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *Mutex) init() { m.once.Do(func() { m.ch = make(chan struct{}, 1) }) }

// Lock waits for the lock until ctx ends, then returns ctx's error.
func (m *Mutex) Lock(ctx context.Context) error {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases a lock taken with Lock.
func (m *Mutex) Unlock() {
	m.init()
	<-m.ch
}
