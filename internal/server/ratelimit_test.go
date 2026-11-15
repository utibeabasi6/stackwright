package server_test

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestTooManyRequestsAreRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	owner := w.setup()
	viewer := w.member(owner, "a", "viewer@a.test", "ROLE_VIEWER")

	var limited atomic.Int32
	for range 40 {
		if _, code := w.client().call("AuthService/SignIn", map[string]any{"email": "nobody@a.test", "password": "wrong"}); code == "resource_exhausted" {
			limited.Add(1)
		}
	}
	if limited.Load() == 0 {
		t.Error("40 sign-ins from one address were all answered")
	}

	limited.Store(0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 60 {
				if _, code := owner.call("ProjectService/ListProjects", map[string]any{"org": "a"}); code == "resource_exhausted" {
					limited.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if limited.Load() == 0 {
		t.Error("480 calls at once from one user were all answered")
	}
	viewer.must("ProjectService/ListProjects", map[string]any{"org": "a"})
}
