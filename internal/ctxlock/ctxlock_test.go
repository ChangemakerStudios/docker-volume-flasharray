package ctxlock

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLockGivesUpWhenContextEnds(t *testing.T) {
	var m Mutex
	if err := m.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline, got %v", err)
	}
	m.Unlock()
	if err := m.Lock(context.Background()); err != nil {
		t.Fatalf("lock not free after Unlock: %v", err)
	}
	m.Unlock()
}

func TestLockExcludes(t *testing.T) {
	var m Mutex
	n := 0
	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 100 {
				_ = m.Lock(context.Background())
				n++
				m.Unlock()
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	if n != 800 {
		t.Fatalf("n = %d, want 800", n)
	}
}
