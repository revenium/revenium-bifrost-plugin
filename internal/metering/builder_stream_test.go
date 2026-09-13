package metering

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/streaming"
)

// streamCtx constructs a real *BifrostContext with KeyRequestStart populated
// + (optionally) all twelve KeyHeader* ctxkeys so identity propagation can be
// asserted in the happy-path row. Reuses the same shape as
// builder_success_test.go.
func streamCtx(start time.Time, withHeaders bool) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(ctxkeys.KeyRequestStart, start)
	if withHeaders {
		setAllHeaders(ctx)
	}
	return ctx
}

// makeState builds an inline *streaming.State with the given metrics.
// Mirrors the planner's guidance to construct fixtures via direct
// Metrics-field assignment (the StreamMetrics struct is exported per
// state.go).
func makeState(firstChunkAt, lastChunkAt time.Time, totalChunks, in, out, total int) *streaming.State {
	s := streaming.New()
	s.Metrics.FirstChunkAt = firstChunkAt
	s.Metrics.LastChunkAt = lastChunkAt
	s.Metrics.TotalChunks = totalChunks
	s.Metrics.InputTokens = in
	s.Metrics.OutputTokens = out
	s.Metrics.TotalTokens = total
	return s
}

// TestBuildStream locks the METER-04 + METER-07 streaming roll-up contract
// per 03-03-PLAN.md <behavior>. Covers: happy path, nil state, zero metrics,
// Delta 2 (WithStreaming overrides WithTiming CompletionStartTime), Pitfall 13
// MiddlewareSource override, METER-07 Phase-4-deferred model/provider
// limitation, zero token defensive path.
func TestBuildStream(t *testing.T) {
	t.Run("happy path — populated state + headers + KeyRequestStart", func(t *testing.T) {
		now := time.Now()
		start := now.Add(-2 * time.Second)
		firstChunkAt := now.Add(-1 * time.Second) // TTFT ≈ 1000 ms
		ctx := streamCtx(start, true)

		state := makeState(firstChunkAt, now, 5, 10, 20, 30)
		state.Metrics.Model = "claude-3-5-sonnet"
		state.Metrics.Provider = "anthropic"
		payload := BuildStream(ctx, state)

		require.NotNil(t, payload, "[METER-04] happy path must return non-nil payload")
		require.True(t, payload.IsStreamed, "[METER-04] IsStreamed=true on streaming roll-up")
		require.Equal(t, "claude-3-5-sonnet", payload.Model,
			"[real-API fix] streaming payload MUST carry model from terminal chat chunk")
		require.Equal(t, "anthropic", payload.Provider,
			"[METER-07] streaming payload MUST carry provider from terminal chat chunk")
		require.Equal(t, "END", payload.StopReason)
		require.Equal(t, MiddlewareSourceGuardrail, payload.MiddlewareSource,
			"[Pitfall 13] MiddlewareSource MUST equal locked constant")
		// TTFT ~ 1000 ms (firstChunkAt - start ≈ 1s). Allow a wide tolerance
		// against scheduling jitter.
		require.GreaterOrEqual(t, payload.TimeToFirstToken, int64(900),
			"TTFT must reflect firstChunkAt - start (~1000ms)")
		require.LessOrEqual(t, payload.TimeToFirstToken, int64(1100),
			"TTFT must reflect firstChunkAt - start (~1000ms)")
		require.EqualValues(t, 10, payload.InputTokenCount)
		require.EqualValues(t, 20, payload.OutputTokenCount)
		require.EqualValues(t, 30, payload.TotalTokenCount)
		require.Equal(t, "CHAT", payload.OperationType)
		// Identity propagation (Delta A).
		require.NotNil(t, payload.Subscriber)
		require.Equal(t, "sub-42", payload.Subscriber["id"])
		require.Equal(t, "Acme Corp", payload.OrganizationName)
		require.Equal(t, "Widgets", payload.ProductName)
		require.Equal(t, "trace-xyz", payload.TraceID)
	})

	t.Run("nil state — caller skips Send (returns nil)", func(t *testing.T) {
		ctx := streamCtx(time.Now(), false)
		payload := BuildStream(ctx, nil)
		require.Nil(t, payload, "[METER-04] nil state -> nil payload (caller skips Send)")
	})

	t.Run("zero metrics — defensive non-nil payload, TTFT=0, tokens=0", func(t *testing.T) {
		ctx := streamCtx(time.Now(), false)
		state := streaming.New() // all-zero Metrics
		payload := BuildStream(ctx, state)

		require.NotNil(t, payload, "defensive: builder must NOT return nil on zero Metrics")
		require.True(t, payload.IsStreamed)
		require.EqualValues(t, 0, payload.TimeToFirstToken,
			"defensive: zero FirstChunkAt -> TTFT=0 (NEVER negative)")
		require.EqualValues(t, 0, payload.InputTokenCount)
		require.EqualValues(t, 0, payload.OutputTokenCount)
		require.EqualValues(t, 0, payload.TotalTokenCount)
	})

	t.Run("Delta 2 — WithStreaming overrides WithTiming CompletionStartTime", func(t *testing.T) {
		// CompletionStartTime MUST track FirstChunkAt, NOT start. Set
		// FirstChunkAt = now-1s; start = now-5s. If the order is wrong,
		// CompletionStartTime would track ≈ start (5s ago).
		now := time.Now()
		start := now.Add(-5 * time.Second)
		firstChunkAt := now.Add(-1 * time.Second)
		ctx := streamCtx(start, false)

		state := makeState(firstChunkAt, now, 3, 5, 5, 10)
		payload := BuildStream(ctx, state)
		require.NotNil(t, payload)

		parsed, err := time.Parse(time.RFC3339, payload.CompletionStartTime)
		require.NoError(t, err, "CompletionStartTime must serialize as RFC3339")
		// CompletionStartTime ≈ firstChunkAt within 1s tolerance.
		require.WithinDuration(t, firstChunkAt, parsed, 1*time.Second,
			"[Delta 2] CompletionStartTime MUST track FirstChunkAt (WithStreaming override)")
		// AND CompletionStartTime must NOT track start (which is 5s earlier).
		require.False(t, parsed.Sub(start).Abs() < 2*time.Second,
			"[Delta 2] CompletionStartTime MUST NOT track WithTiming start (call order regression)")
	})

	t.Run("Pitfall 13 — MiddlewareSource override (NOT SDK default)", func(t *testing.T) {
		ctx := streamCtx(time.Now(), false)
		state := makeState(time.Now(), time.Now(), 1, 1, 1, 2)
		payload := BuildStream(ctx, state)
		require.NotNil(t, payload)
		require.NotEqual(t, "revenium-go-sdk", payload.MiddlewareSource,
			"[Pitfall 13] SDK default must be overridden")
		require.Equal(t, "GUARDRAIL", payload.MiddlewareSource,
			"[Pitfall 13] MiddlewareSource MUST equal locked constant")
	})

	t.Run("METER-07 — streaming emits model + provider from finalized state", func(t *testing.T) {
		ctx := streamCtx(time.Now(), false)
		state := makeState(time.Now(), time.Now(), 1, 1, 1, 2)
		state.Metrics.Model = "gpt-4o-mini"
		state.Metrics.Provider = "openai"
		payload := BuildStream(ctx, state)
		require.NotNil(t, payload)
		// The previously-inverted require.Empty assertion codified the bug
		// where streaming metered model="" and 400'd against the real API.
		// BuildStream now emits the model/provider threaded from the terminal
		// chat chunk via streaming.State.
		require.Equal(t, "gpt-4o-mini", payload.Model,
			"[METER-07] streaming emits the finalized model — no longer blank")
		require.Equal(t, "openai", payload.Provider,
			"[METER-07] streaming emits the finalized provider — no longer blank")
	})

	t.Run("zero token counts — no panic", func(t *testing.T) {
		ctx := streamCtx(time.Now(), false)
		state := makeState(time.Now(), time.Now(), 0, 0, 0, 0)
		require.NotPanics(t, func() {
			payload := BuildStream(ctx, state)
			require.NotNil(t, payload)
			require.EqualValues(t, 0, payload.InputTokenCount)
			require.EqualValues(t, 0, payload.OutputTokenCount)
			require.EqualValues(t, 0, payload.TotalTokenCount)
		})
	})
}
