//go:build pluginsanity

package main

// TestPostLLMHook_Phase3FiveBranchDispatch locks Plan 03-02's PostLLMHook
// 5-branch dispatch contract end-to-end. Drives the four PostLLMHook
// outcomes (success/failure/blocked/streaming-noop) against a fake httptest
// metering server and asserts the exact (send_count, stop_reason) tuple per
// row. The critical METER-03 anti-double-count row (Row 3) AND the METER-05
// streaming-no-op row (Row 4) lock the dispatch ordering against regression.
//
// Row coverage:
//   - Row 1 (P03-02-R01 / METER-01 + METER-06): success path emits ONE END.
//   - Row 2 (P03-02-R02 / METER-02): failure path emits ONE ERROR.
//   - Row 3 (P03-02-R03 / METER-03 + Pitfall 1): blocked-with-success-shaped-resp
//     emits EXACTLY ONE BUDGET_EXCEEDED — NEVER one block + one zero-token
//     success. This is the regression lock on the dispatch order: the block
//     branch must early-return before the success branch can fire.
//   - Row 4 (P03-02-R04 / METER-05 + Pitfall 2): streaming PostLLMHook emits
//     ZERO events; the StreamChunkHook terminal branch (Plan 03-03) owns
//     roll-up. If this row's count ever > 0, double-emission has regressed.
//
// Invocation:
//   REVENIUM_METERING_API_KEY=dummy \
//     go test -tags pluginsanity -count=1 \
//       -run TestPostLLMHook_Phase3FiveBranchDispatch -v .
//
// The //go:build pluginsanity tag keeps this file out of the default
// `go test ./...` path (matches Plan 01-08 + 02-02 + 02-04 precedent).
//
// Package-state save/restore (Pattern D LIFO) matches plugin_phase2_test.go
// L53-60 + plugin_recover_test.go L96-101: every package var the test
// mutates (logger, meteringClient) is saved at the outer scope and restored
// via t.Cleanup so a test failure cannot leak modified state into a later
// subtest or test-binary run.
//
// Production-path discipline (Pattern C — Plan 02-04 D-23): the
// *metering.Client is constructed via the production metering.New(cfg)
// constructor with BaseURL rewritten to httptest.NewServer.URL. NO seam
// interface. NO mock injection. The production *http.Client + payload
// marshalling + goroutine + WaitGroup paths are ALL exercised.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	revsdk "github.com/revenium/revenium-go-sdk/core/metering"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/budget"
	"github.com/revenium/revenium-bifrost-plugin/internal/config"
	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/metering"
	"github.com/revenium/revenium-bifrost-plugin/internal/plog"
)

func TestPostLLMHook_Phase3FiveBranchDispatch(t *testing.T) {
	// Save-restore the package-level state the test mutates. Pattern D LIFO
	// via t.Cleanup; matches plugin_phase2_test.go convention.
	origLogger := logger
	origMetering := meteringClient
	t.Cleanup(func() {
		logger = origLogger
		meteringClient = origMetering
	})

	tests := []struct {
		name           string
		reqID          string
		setupCtx       func(*schemas.BifrostContext)
		resp           *schemas.BifrostResponse
		bifrostErr     *schemas.BifrostError
		wantSendCount  int32
		wantStopReason string
	}{
		{
			name:  "success_emits_END",
			reqID: "P03-02-R01",
			setupCtx: func(ctx *schemas.BifrostContext) {
				// Exercise identity assignment by populating a few headers.
				ctx.SetValue(ctxkeys.KeyHeaderSubscriberID, "sub-r01")
				ctx.SetValue(ctxkeys.KeyHeaderOrgName, "Acme")
				ctx.SetValue(ctxkeys.KeyHeaderProductName, "Widgets")
				ctx.SetValue(ctxkeys.KeyHeaderTraceID, "trace-r01")
			},
			resp: &schemas.BifrostResponse{
				ChatResponse: &schemas.BifrostChatResponse{
					ID:    "resp_001",
					Model: "gpt-4o",
					Usage: &schemas.BifrostLLMUsage{
						PromptTokens:     10,
						CompletionTokens: 20,
						TotalTokens:      30,
					},
					ExtraFields: schemas.BifrostResponseExtraFields{
						Provider: schemas.OpenAI,
					},
				},
			},
			bifrostErr:     nil,
			wantSendCount:  1,
			wantStopReason: "END",
		},
		{
			name:     "failure_emits_ERROR",
			reqID:    "P03-02-R02",
			setupCtx: nil,
			resp:     nil,
			bifrostErr: &schemas.BifrostError{
				EventID: ptrString("evt_002"),
				Error:   &schemas.ErrorField{Message: "rate limited"},
				ExtraFields: schemas.BifrostErrorExtraFields{
					Provider: schemas.Anthropic,
				},
			},
			wantSendCount:  1,
			wantStopReason: "ERROR",
		},
		{
			// METER-03 + Pitfall 1: a blocked request MUST emit ONE
			// BUDGET_EXCEEDED event, NEVER one block + one zero-token
			// success. The resp field is intentionally populated to assert
			// the block branch wins even when a downstream success-shaped
			// resp is also present.
			name:  "blocked_emits_BUDGET_EXCEEDED_only",
			reqID: "P03-02-R03",
			setupCtx: func(ctx *schemas.BifrostContext) {
				ctx.SetValue(ctxkeys.KeyShortCircuited, &budget.Decision{
					Allowed:       false,
					ExceededCount: 1,
					Budget: &budget.Budget{
						Name:         "test-budget",
						Threshold:    100.0,
						CurrentValue: 105.0,
						PercentUsed:  105.0,
						Risk:         "EXCEEDED",
					},
				})
			},
			resp: &schemas.BifrostResponse{
				// Success-shaped resp present alongside the block — the
				// dispatch order MUST short-circuit at the block branch
				// BEFORE this resp is seen by BuildSuccess.
				ChatResponse: &schemas.BifrostChatResponse{
					ID:    "resp_003",
					Model: "gpt-4o",
					Usage: &schemas.BifrostLLMUsage{
						PromptTokens:     10,
						CompletionTokens: 20,
						TotalTokens:      30,
					},
					ExtraFields: schemas.BifrostResponseExtraFields{
						Provider: schemas.OpenAI,
					},
				},
			},
			bifrostErr:     nil,
			wantSendCount:  1,
			wantStopReason: "BUDGET_EXCEEDED",
		},
		{
			// METER-05 + Pitfall 2: streaming PostLLMHook MUST NOT emit;
			// the StreamChunkHook terminal branch (Plan 03-03) owns roll-up.
			// If this row's count ever > 0, double-emission has regressed.
			name:  "streaming_emits_zero",
			reqID: "P03-02-R04",
			setupCtx: func(ctx *schemas.BifrostContext) {
				ctx.SetValue(ctxkeys.KeyRequestType, schemas.ChatCompletionStreamRequest)
			},
			resp: &schemas.BifrostResponse{
				// "Premature" PostLLMHook resp on the streaming path — must
				// NOT trigger emission.
				ChatResponse: &schemas.BifrostChatResponse{
					ID:    "resp_004",
					Model: "gpt-4o",
					Usage: &schemas.BifrostLLMUsage{
						PromptTokens:     10,
						CompletionTokens: 20,
						TotalTokens:      30,
					},
					ExtraFields: schemas.BifrostResponseExtraFields{
						Provider: schemas.OpenAI,
					},
				},
			},
			bifrostErr:     nil,
			wantSendCount:  0,
			wantStopReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sendCount atomic.Int32
			var captured atomic.Pointer[revsdk.MeteringPayload]

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sendCount.Add(1)
				var p revsdk.MeteringPayload
				_ = json.NewDecoder(r.Body).Decode(&p)
				captured.Store(&p)
				w.WriteHeader(200)
			}))
			t.Cleanup(srv.Close)

			cfg := &config.Config{
				BaseURL:            srv.URL,
				APIKey:             "test",
				BudgetCheckTimeout: 3 * time.Second,
			}
			mc, err := metering.New(cfg)
			require.NoError(t, err, "[%s] metering.New must succeed against fake server", tt.reqID)

			logger = plog.New(false)
			meteringClient = mc

			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
			if tt.setupCtx != nil {
				tt.setupCtx(ctx)
			}

			_, _, err = PostLLMHook(ctx, tt.resp, tt.bifrostErr)
			require.NoError(t, err, "[%s] PostLLMHook must not return error", tt.reqID)

			// Synchronous flush (Pitfall 11 / METER-06): Close blocks until
			// all in-flight Send goroutines drain, so the server-side count
			// is observable before assertion.
			require.NoError(t, mc.Close(), "[%s] meteringClient.Close must succeed", tt.reqID)

			require.Equal(t, tt.wantSendCount, sendCount.Load(),
				"[%s] expected Send count mismatch (METER-03 anti-double-count + METER-05 streaming-no-op live or die here)",
				tt.reqID)

			if tt.wantStopReason != "" {
				p := captured.Load()
				require.NotNil(t, p, "[%s] expected a captured metering payload", tt.reqID)
				require.Equal(t, tt.wantStopReason, p.StopReason,
					"[%s] stop_reason mismatch", tt.reqID)
			}
		})
	}
}

// ptrString returns &s for use in test fixtures that need *string.
func ptrString(s string) *string { return &s }
