//go:build integration

// fake_revenium.go is the integration-suite's fake Revenium control-plane
// server. NEW in Plan 04-03 — there is NO analogous file in spike/contract/
// (the spike only proved the Bifrost-side contract; it did not exercise
// the Revenium budget-check or metering-POST paths).
//
// Responsibilities:
//
//  1. GET /v2/api/virtual-keys/budget-check?keyHash=... — returns a JSON body
//     matching the production budget-check response shape (verified against
//     internal/budget/client.go's anonymous-struct decoder: top-level
//     {"summary":{"exceededCount":N},"items":[{name,threshold,...,risk}]}).
//     The exceededCount is per-keyHash, set by tests via setBudgetBlock().
//     Default (no setBudgetBlock call for a keyHash) is 0 → allow.
//
//  2. POST /meter/v2/ai/completions — accepts the revenium-go-sdk
//     MeteringClient's POST body, decodes it into a thread-safe slice of
//     map[string]any, returns 202 Accepted (matching the SDK's expectation).
//     Tests read the captured payloads via the closure returned from
//     StartFakeRevenium to assert on stop_reason / middlewareSource / TTFT.
//
// All other routes → 404 (catch-all guard against accidental URL drift).
//
// Sub-module isolation discipline (D-04): this file does NOT import any
// production package — neither revenium-go-sdk types nor the parent
// module's internal subtree. Captured payloads are decoded into untyped
// map[string]any so the integration suite stays structurally independent
// of the SDK's Go struct shape and only depends on the JSON wire contract
// (which is what real customers depend on too).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
)

// StartFakeRevenium spins up an httptest.Server that serves the two
// production endpoints the plugin calls:
//
//   - GET /v2/api/virtual-keys/budget-check (consumed by
//     internal/budget/client.go)
//   - POST /meter/v2/ai/completions (consumed by revenium-go-sdk's
//     MeteringClient — verified path at
//     github.com/revenium/revenium-go-sdk/core/metering/url.go)
//
// Returns four closures so tests can drive + observe state without
// reaching into package-globals:
//
//   - baseURL is the http://127.0.0.1:PORT root, suitable for
//     REVENIUM_METERING_BASE_URL.
//   - getPayloads returns a COPY of all captured POST bodies (mutex-guarded
//     snapshot, safe across goroutines).
//   - setBudgetBlock sets the exceededCount for a given keyHash so the
//     budget-check endpoint returns "blocked" for subsequent calls.
//   - shutdown closes the underlying httptest.Server.
func StartFakeRevenium() (baseURL string, getPayloads func() []map[string]any, setBudgetBlock func(keyHash string, exceededCount int), shutdown func()) {
	var payloadsMu sync.Mutex
	capturedPayloads := []map[string]any{}

	var blockMu sync.Mutex
	blockedKeyHashCount := map[string]int{}

	mux := http.NewServeMux()

	// GET /v2/api/virtual-keys/budget-check — mirrors the production
	// endpoint shape exactly per internal/budget/client.go anonymous-struct
	// decoder. Note the production decoder reads BOTH the `summary.exceededCount`
	// integer AND the `items[]` array of {name, threshold, currentValue,
	// remaining, percentUsed, risk}. To trigger the BUDGET-03 short-circuit
	// path with a structured body (risk == "EXCEEDED"), the blocked branch
	// includes one fully-populated item.
	mux.HandleFunc("/v2/api/virtual-keys/budget-check", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		keyHash := r.URL.Query().Get("keyHash")

		blockMu.Lock()
		count := blockedKeyHashCount[keyHash]
		blockMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if count > 0 {
			// Blocked branch: exceededCount > 0 AND items[] has one
			// risk == "EXCEEDED" entry so the production decoder lifts
			// it into Decision.Budget (BUDGET-03 verified shape).
			_, _ = w.Write([]byte(`{"summary":{"exceededCount":` + intToStr(count) + `},"items":[{"name":"daily","threshold":100,"currentValue":150,"remaining":-50,"percentUsed":150,"risk":"EXCEEDED"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"summary":{"exceededCount":0},"items":[]}`))
	})

	// POST /meter/v2/ai/completions — matches revenium-go-sdk's
	// MeteringClient.SendSync POST target (verified path at
	// core/metering/url.go: base + "/meter/v2/ai/completions"). Decode body
	// into a generic map and append under mutex; return 202 Accepted to
	// match the SDK's expectation. NEVER block — the SDK's Send() is
	// fire-and-forget, but slow responses here would skew TTFT measurements
	// if the client retries.
	mux.HandleFunc("/meter/v2/ai/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		// Best-effort decode; an empty or malformed body still appends as
		// nil so the test can spot the failure mode. The SDK never sends
		// malformed JSON, but defensive coding here makes a future regression
		// obvious in the captured-payload slice.
		_ = json.NewDecoder(r.Body).Decode(&body)

		payloadsMu.Lock()
		capturedPayloads = append(capturedPayloads, body)
		payloadsMu.Unlock()

		w.WriteHeader(http.StatusAccepted)
	})

	srv := httptest.NewServer(mux)

	getPayloads = func() []map[string]any {
		payloadsMu.Lock()
		defer payloadsMu.Unlock()
		// Return a copy so callers can iterate without holding the lock.
		out := make([]map[string]any, len(capturedPayloads))
		copy(out, capturedPayloads)
		return out
	}

	setBudgetBlock = func(keyHash string, exceededCount int) {
		blockMu.Lock()
		defer blockMu.Unlock()
		blockedKeyHashCount[keyHash] = exceededCount
	}

	shutdown = srv.Close
	return srv.URL, getPayloads, setBudgetBlock, shutdown
}

// intToStr is a small helper to avoid pulling strconv into the package for
// the single use site above. Keeps the file's import list minimal and the
// budget-check body templating obvious to a reader.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
