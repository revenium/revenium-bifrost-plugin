//go:build pluginsanity

package main

// TestPreLLMHook_Phase2EndToEnd locks the Plan 02-04 Phase 2 keystone
// contract: the full HTTPTransportPreHook -> PreLLMHook chain drives the
// four PreLLMHook outcomes (skipped / allowed / blocked / fail-open) against
// an in-process httptest fake budget endpoint, AND the Phase 3 reader
// contract (ctxkeys.KeyShortCircuited payload type == *budget.Decision) is
// type-asserted explicitly on the block row so any regression of the locked
// invariant fails LOUDLY at PR time.
//
// Row coverage per D-22 (reqID traceability):
//   - Row A (BUDGET-01): no key-hash header -> skip + allow + no HTTP call.
//   - Row B (BUDGET-02): allowed (HTTP 200 + summary.exceededCount=0).
//   - Row C (BUDGET-03 + IDENTITY-01): blocked (HTTP 200 + exceededCount>0)
//     with a MIXED-CASE x-revenium-key-hash header. Exercises Plan 02-02's
//     *HTTPRequest.CaseInsensitiveHeaderLookup AND asserts the Phase 3
//     contract: ctx.Value(KeyShortCircuited).(*budget.Decision) round-trips
//     with d.Allowed == false and d.Budget populated.
//   - Row D (BUDGET-04/05): fake server HTTP 500 -> *budget.ErrTransport ->
//     fail-open passthrough.
//
// Invocation: `go test -tags pluginsanity -run TestPreLLMHook_Phase2EndToEnd
// -count=1 .` — the //go:build pluginsanity tag keeps this file out of the
// default `go test ./...` path (matches Plan 01-08 + 02-02 precedent).
//
// Package-state save/restore matches plugin_recover_test.go L91-101: every
// package var the test mutates (logger, cfg, budgetClient) is saved at the
// outer scope and restored via t.Cleanup so a test failure cannot leak
// modified state into a later subtest or test-binary run.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/budget"
	"github.com/revenium/revenium-bifrost-plugin/internal/config"
	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/plog"
)

func TestPreLLMHook_Phase2EndToEnd(t *testing.T) {
	// Save and defer-restore the package-level state mutated by the test.
	// LIFO via t.Cleanup; matches plugin_recover_test.go convention.
	origLogger := logger
	origCfg := cfg
	origBudget := budgetClient
	t.Cleanup(func() {
		logger = origLogger
		cfg = origCfg
		budgetClient = origBudget
	})

	tests := []struct {
		name           string
		reqID          string
		headers        map[string]string
		srvStatusCode  int    // HTTP status the fake server returns
		srvResponse    string // body the fake server returns (empty for skipped row)
		wantSC         bool   // expect *schemas.LLMPluginShortCircuit != nil
		wantBlocked    bool   // expect ctx.Value(KeyShortCircuited) to be *budget.Decision{Allowed:false}
		wantServerHits int32  // expected fake-server hit count (0 for the skip row)
	}{
		{
			name:           "no key-hash header — skip + allow",
			reqID:          "BUDGET-01",
			headers:        map[string]string{},
			srvStatusCode:  200, // unused — server should not be hit
			srvResponse:    "",
			wantSC:         false,
			wantBlocked:    false,
			wantServerHits: 0,
		},
		{
			name:           "allowed — pass through",
			reqID:          "BUDGET-02",
			headers:        map[string]string{"x-revenium-key-hash": "sk-test-ok"},
			srvStatusCode:  200,
			srvResponse:    `{"summary":{"exceededCount":0}}`,
			wantSC:         false,
			wantBlocked:    false,
			wantServerHits: 1,
		},
		{
			name:           "blocked — 429 short-circuit with mixed-case header",
			reqID:          "BUDGET-03+IDENTITY-01",
			headers:        map[string]string{"X-Revenium-Key-Hash": "sk-test-block"}, // MIXED case
			srvStatusCode:  200,
			srvResponse:    `{"summary":{"exceededCount":1},"items":[{"name":"x","threshold":1,"currentValue":2,"remaining":-1,"percentUsed":200,"risk":"EXCEEDED"}]}`,
			wantSC:         true,
			wantBlocked:    true,
			wantServerHits: 1,
		},
		{
			name:           "5xx — fail open",
			reqID:          "BUDGET-04/05",
			headers:        map[string]string{"x-revenium-key-hash": "sk-test-5xx"},
			srvStatusCode:  500,
			srvResponse:    "", // body absent on fail-open; PreLLMHook never decodes a 500 response
			wantSC:         false,
			wantBlocked:    false,
			wantServerHits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Per-row atomic hit counter — proves Row A (BUDGET-01)
			// never hits the budget endpoint and Rows B-D hit exactly
			// once per invocation.
			var hitCount atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hitCount.Add(1)
				w.WriteHeader(tt.srvStatusCode)
				if tt.srvResponse != "" {
					_, _ = w.Write([]byte(tt.srvResponse))
				}
			}))
			t.Cleanup(srv.Close)

			// Reassign package-level state to point at the fake server.
			// The production *http.Client (constructed in budget.New) is
			// the test *http.Client — closest-possible production-path
			// coverage per D-23.
			cfg = &config.Config{
				BaseURL:            srv.URL,
				APIKey:             "test",
				BudgetCheckTimeout: 3 * time.Second,
			}
			logger = plog.New(false)
			budgetClient = budget.New(cfg, logger)
			t.Cleanup(budgetClient.Close)

			// Real *BifrostContext — ctx.SetValue / ctx.Value round-trip
			// works (used by HTTPTransportPreHook to publish KeyHeaderKeyHash
			// and by PreLLMHook to read it via identity.ExtractKeyHash and
			// write KeyShortCircuited / KeyDryRunDecision on the block path).
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

			// Drive the production hook chain in the order Bifrost dispatches:
			// HTTPTransportPreHook (header -> ctxkey publishing) -> PreLLMHook
			// (budget orchestrator). Both passthrough returns from
			// HTTPTransportPreHook are discarded — Plan 02-02 contract.
			httpReq := &schemas.HTTPRequest{Headers: tt.headers}
			_, _ = HTTPTransportPreHook(ctx, httpReq)

			req := &schemas.BifrostRequest{}
			outReq, sc, err := PreLLMHook(ctx, req)

			require.NoError(t, err, "[%s]", tt.reqID)
			require.Same(t, req, outReq, "[%s] outReq must be the original *BifrostRequest pointer", tt.reqID)

			if tt.wantSC {
				require.NotNil(t, sc, "[%s] expected non-nil short-circuit", tt.reqID)
				require.NotNil(t, sc.Error, "[%s] sc.Error must be non-nil", tt.reqID)
				require.NotNil(t, sc.Error.StatusCode, "[%s] sc.Error.StatusCode must be non-nil", tt.reqID)
				require.Equal(t, 429, *sc.Error.StatusCode, "[%s] block must surface HTTP 429", tt.reqID)
				require.NotNil(t, sc.Error.AllowFallbacks, "[%s] sc.Error.AllowFallbacks must be non-nil", tt.reqID)
				require.False(t, *sc.Error.AllowFallbacks, "[%s] AllowFallbacks must be &false (no provider fallback on block)", tt.reqID)
			} else {
				require.Nil(t, sc, "[%s] no short-circuit expected", tt.reqID)
			}

			// Phase 3 contract assertion: KeyShortCircuited payload type
			// MUST be *budget.Decision (NOT bool, NOT generic struct).
			// This is the LOCKED Phase 3 reader contract — Phase 3's
			// PostLLMHook will type-assert via comma-ok back to
			// *budget.Decision and dispatch metering.BuildBlocked.
			if tt.wantBlocked {
				v := ctx.Value(ctxkeys.KeyShortCircuited)
				require.NotNil(t, v, "[%s] KeyShortCircuited must be set on the block path", tt.reqID)
				d, ok := v.(*budget.Decision)
				require.True(t, ok, "[%s] KeyShortCircuited payload must be *budget.Decision (Phase 3 contract)", tt.reqID)
				require.NotNil(t, d, "[%s] KeyShortCircuited *budget.Decision must be non-nil", tt.reqID)
				require.False(t, d.Allowed, "[%s] blocked Decision must have Allowed=false", tt.reqID)
				require.NotNil(t, d.Budget, "[%s] blocked Decision must carry a populated Budget", tt.reqID)
				require.Equal(t, "x", d.Budget.Name, "[%s] Budget.Name must round-trip from fake response", tt.reqID)
				require.Equal(t, "EXCEEDED", d.Budget.Risk, "[%s] Budget.Risk must round-trip from fake response", tt.reqID)
			} else {
				require.Nil(t, ctx.Value(ctxkeys.KeyShortCircuited),
					"[%s] KeyShortCircuited must NOT be set when no block occurred", tt.reqID)
			}

			require.Equal(t, tt.wantServerHits, hitCount.Load(),
				"[%s] fake server hit count mismatch", tt.reqID)
		})
	}
}
