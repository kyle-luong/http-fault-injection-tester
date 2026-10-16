package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kyle-luong/http-fault-injection-tester/internal/client"
	"github.com/kyle-luong/http-fault-injection-tester/internal/faults"
	"github.com/kyle-luong/http-fault-injection-tester/internal/fixture"
	"github.com/kyle-luong/http-fault-injection-tester/internal/proxy"
)

// BenchmarkOperation measures one no-fault operation against the built-in
// fixture, directly and through the proxy, at three parallelism levels. The
// difference between the two is the proxy's added latency, which is the PRD's
// GO-AC13 evidence (and GO-T15).
//
// Benchmarks do not run under `go test`, so this costs CI nothing. Run it with
// ./scripts/benchmark.sh, which records the host conditions the numbers depend
// on. Never measure a -race build.
//
// Both paths send a byte-identical request, including the harness control
// headers the proxy requires, so the only difference is one extra loopback hop
// and the proxy's own work. Control traffic -- the per-case reset -- always
// goes straight to the fixture and is never measured.
func BenchmarkOperation(b *testing.B) {
	for _, parallel := range []int{1, 8, 32} {
		fx, err := fixture.Start(fixture.ModeUnsafe)
		if err != nil {
			b.Fatalf("start fixture: %v", err)
		}
		schedule, err := faults.NewSchedule(nil)
		if err != nil {
			b.Fatalf("new schedule: %v", err)
		}
		prx, err := proxy.Start(proxy.Options{
			Upstream:        fx.BaseURL(),
			Schedule:        schedule,
			UpstreamTimeout: 10 * time.Second,
		})
		if err != nil {
			b.Fatalf("start proxy: %v", err)
		}

		for _, target := range []struct{ name, baseURL string }{
			{"direct", fx.BaseURL()},
			{"proxy", prx.BaseURL()},
		} {
			b.Run(fmt.Sprintf("%s/parallel=%d", target.name, parallel), func(b *testing.B) {
				benchmarkOperations(b, target.baseURL, fx.BaseURL(), parallel)
			})
		}

		if err := prx.Close(); err != nil {
			b.Errorf("close proxy: %v", err)
		}
		if err := fx.Close(); err != nil {
			b.Errorf("close fixture: %v", err)
		}
	}
}

// benchmarkOperations drives b.N operations through baseURL with the given
// number of workers, and reports the latency distribution rather than the mean
// alone: a mean hides exactly the tail a proxy adds.
func benchmarkOperations(b *testing.B, baseURL, fixtureURL string, parallel int) {
	b.Helper()

	hc := client.NewHTTPClient()
	defer client.CloseIdleConnections(hc)

	// One case per worker, reset up front so the measured requests are
	// operations and nothing else.
	caseIDs := make([]string, parallel)
	for i := range parallel {
		caseIDs[i] = fmt.Sprintf("bench-%02d", i)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := fixture.Reset(ctx, hc, fixtureURL, caseIDs[i]); err != nil {
			cancel()
			b.Fatalf("reset %s: %v", caseIDs[i], err)
		}
		cancel()
	}

	// One warm operation per worker, so establishing a connection is not inside
	// the measured window. The proxy path would otherwise pay for two dials
	// where the direct path pays for one, which inflates its tail at high
	// parallelism -- a bias in favour of whichever path is being argued against.
	for i := range parallel {
		if err := sendOperation(hc, baseURL, caseIDs[i], -1-i); err != nil {
			b.Fatalf("warm up %s: %v", caseIDs[i], err)
		}
	}

	latencies := make([][]time.Duration, parallel)
	var failures int64
	var mu sync.Mutex

	b.ReportAllocs()
	b.ResetTimer()
	started := time.Now()

	var wg sync.WaitGroup
	wg.Add(parallel)
	for worker := range parallel {
		go func() {
			defer wg.Done()
			mine := make([]time.Duration, 0, b.N/parallel+1)
			// Workers take a strided share of b.N, so every operation is done
			// exactly once however b.N divides.
			for n := worker; n < b.N; n += parallel {
				start := time.Now()
				err := sendOperation(hc, baseURL, caseIDs[worker], n)
				mine = append(mine, time.Since(start))
				if err != nil {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}
			latencies[worker] = mine
		}()
	}
	wg.Wait()

	elapsed := time.Since(started)
	b.StopTimer()

	if failures > 0 {
		b.Fatalf("%d of %d operations failed; a failed request is not a measurement", failures, b.N)
	}

	all := make([]time.Duration, 0, b.N)
	for _, l := range latencies {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "ops/sec")
	// Wall time for the trial, which the PRD asks to see recorded alongside
	// the rate it is derived from.
	b.ReportMetric(float64(elapsed.Milliseconds()), "wall-ms")
	b.ReportMetric(float64(mean(all).Microseconds()), "mean-us")
	b.ReportMetric(float64(quantile(all, 0.50).Microseconds()), "p50-us")
	b.ReportMetric(float64(quantile(all, 0.95).Microseconds()), "p95-us")
	// Sampled while both servers are still up, so it includes whatever the
	// proxy keeps running. It is a count, not a leak check: leaktest covers
	// that in the functional tests.
	b.ReportMetric(float64(runtime.NumGoroutine()), "goroutines")
}

// sendOperation performs one operation with a distinct idempotency key, so no
// request is deduplicated and every one does the fixture's full work.
func sendOperation(hc *http.Client, baseURL, caseID string, n int) error {
	body := []byte(`{"case_id":"` + caseID + `","delta":1}`)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/operation", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(fixture.IdempotencyKeyHeader, fmt.Sprintf("%s-%d", caseID, n))
	req.Header.Set(proxy.HeaderCaseID, caseID)
	req.Header.Set(proxy.HeaderAttempt, "1")
	req.ContentLength = int64(len(body))

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, fixture.MaxResponseBodyBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func mean(sorted []time.Duration) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return total / time.Duration(len(sorted))
}

// quantile returns the nearest-rank value, which needs no interpolation and no
// claim about a distribution this does not know.
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
