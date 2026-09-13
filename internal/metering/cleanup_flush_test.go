package metering

// TestClient_Close_SynchronousFlushOf10InFlight regression-locks Pitfall 11
// (lost shutdown metering) at the 10-concurrent-Send granularity. Phase 1
// spike D-16.5 established the process-level half of the contract by
// measuring SIGTERM-to-Cleanup delta = 2.115383s — proving Bifrost awaits
// the synchronous Cleanup body's return before tearing down the process
// (CONTRACT-05). This test locks the same contract at the in-process
// granularity: 10 concurrent Send goroutines in flight when Close() is
// called, every one of them MUST complete before Close() returns.
//
// METER-06 contract: a single revsdk.MeteringClient instance is reused for
// the plugin's lifetime; Send is fire-and-forget (the SDK manages the
// goroutine + wg.Add(1) internally); Close must wrap Flush which is
// wg.Wait. If a future refactor wraps Close in `go c.inner.Close()` or
// drops the Flush call, this test fails LOUDLY.
//
// Why the slow-server discriminator matters: the pre-Close assertion
// `require.Less(t, hitCount.Load(), int32(10), ...)` is what proves the
// in-flight condition is real. Without it, a fast server could complete
// all 10 Send goroutines BEFORE Close is even called and the test would
// falsely pass even if Close were non-synchronous. The 100ms sleep in the
// fake server forces goroutine in-flight at Close-time.
//
// Pattern C production-path discipline (D-23): the production *Client IS
// the test instance — constructed via the real metering.New(cfg) with
// BaseURL rewritten to httptest.NewServer.URL. No injected seam.
// httptest is the seam.
//
// Pattern D LIFO cleanup: srv via t.Cleanup, NOT defer.

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	revsdk "github.com/revenium/revenium-go-sdk/core/metering"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/config"
)

func TestClient_Close_SynchronousFlushOf10InFlight(t *testing.T) {
	// Atomic hit counter is the single source of truth for both pre-Close
	// (must be < 10) and post-Close (must be == 10) assertions.
	var hitCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 100ms slow-server sleep forces Send goroutines to be in-flight
		// when Close is called. Without this sleep, all 10 could drain
		// before the pre-Close assertion executes — the test would still
		// pass post-Close but the in-flight condition would not be proven.
		time.Sleep(100 * time.Millisecond)
		hitCount.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		BaseURL:            srv.URL,
		APIKey:             "test",
		BudgetCheckTimeout: 3 * time.Second,
	}
	c, err := New(cfg)
	require.NoError(t, err)

	// Fire 10 concurrent Send calls. Send is fire-and-forget — the SDK's
	// Send does wg.Add(1) inside, spawning a goroutine that the SDK's
	// WaitGroup tracks. By the time this loop returns, all 10 goroutines
	// are spawned but blocked inside the slow-server roundtrip.
	for i := 0; i < 10; i++ {
		c.Send(&revsdk.MeteringPayload{Model: "test", Provider: "test"})
	}

	// Pre-Close discriminator: NOT all 10 should have arrived yet (slow
	// server). This is what proves the in-flight condition is real — if
	// hitCount were already 10 here, the post-Close assertion would
	// falsely pass even if Close were async/non-flushing.
	require.Less(t, hitCount.Load(), int32(10),
		"pre-Close hit count must be < 10 (slow server proves Cleanup actually waited)")

	start := time.Now()
	require.NoError(t, c.Close()) // synchronous — must wg.Wait under the hood (Pitfall 11)
	elapsed := time.Since(start)

	// Post-Close lock: every in-flight Send goroutine must have completed
	// before Close returned. THIS is the regression-lock for METER-06
	// synchronous-flush. A future refactor that makes Close async (e.g.,
	// wrapping in `go c.inner.Close()`) fails this assertion.
	require.Equal(t, int32(10), hitCount.Load(),
		"Close MUST block until all 10 in-flight Send goroutines completed (Pitfall 11)")

	// Wall-time assertion: Close must have blocked for at least one
	// slow-server response cycle (100ms). If Close returned in < 100ms,
	// the wg.Wait was effectively skipped.
	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond,
		"Close MUST have blocked for at least one slow-server response cycle")
}
