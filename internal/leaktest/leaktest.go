// Package leaktest detects goroutines that outlive a test.
//
// A fault-injection harness that leaks goroutines or listeners will eventually
// hang a suite, so every test that starts a server asserts cleanup. The check
// is deliberately simple: compare goroutine counts, allowing time for runtime
// and net/http teardown to finish, and dump all stacks on failure.
package leaktest

import (
	"runtime"
	"testing"
	"time"
)

const (
	settleTimeout  = 3 * time.Second
	settleInterval = 20 * time.Millisecond
)

// Check snapshots the goroutine count and registers a cleanup that fails the
// test if the count has not returned to its starting level.
//
// Call it as the first statement of a test, before starting any server:
//
//	func TestThing(t *testing.T) {
//	    leaktest.Check(t)
//	    srv := startServer(t) // registers its own t.Cleanup
//	    ...
//	}
//
// Cleanups run last-registered-first, so registering this one first means it
// runs after every server has been shut down. Using a deferred call instead
// would check before those shutdowns and report false leaks.
func Check(t testing.TB) {
	t.Helper()
	before := runtime.NumGoroutine()

	t.Cleanup(func() {
		deadline := time.Now().Add(settleTimeout)
		for {
			after := runtime.NumGoroutine()
			if after <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				n := runtime.Stack(buf, true)
				t.Errorf("goroutine leak: %d at start, %d after waiting %s\n%s",
					before, after, settleTimeout, buf[:n])
				return
			}
			time.Sleep(settleInterval)
		}
	})
}
