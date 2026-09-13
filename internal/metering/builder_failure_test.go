package metering

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
)

// ptrString is a tiny helper that returns *string for inline test fixtures.
func ptrString(s string) *string { return &s }

// TestBuildFailure locks the METER-02 ERROR path and the eight subtests
// defined in 03-01-PLAN.md <behavior>.
func TestBuildFailure(t *testing.T) {
	t.Run("happy failure path — EventID + error message + provider", func(t *testing.T) {
		ctx := newTestCtx()
		setAllHeaders(ctx)
		start := time.Now().Add(-500 * time.Millisecond)
		ctx.SetValue(ctxkeys.KeyRequestStart, start)

		bifrostErr := &schemas.BifrostError{
			EventID: ptrString("evt_xyz"),
			Error:   &schemas.ErrorField{Message: "rate limit exceeded"},
			ExtraFields: schemas.BifrostErrorExtraFields{
				Provider:               "openai",
				ResolvedModelUsed:      "gpt-4o-mini",
				OriginalModelRequested: "gpt-4o-mini",
			},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload, "[METER-02] happy failure path returns non-nil payload")

		require.Equal(t, "ERROR", payload.StopReason)
		require.Equal(t, "evt_xyz", payload.TransactionID)
		require.EqualValues(t, 0, payload.InputTokenCount)
		require.EqualValues(t, 0, payload.OutputTokenCount)
		require.EqualValues(t, 0, payload.TotalTokenCount)
		require.Equal(t, "openai", payload.Provider)
		require.Equal(t, "gpt-4o-mini", payload.Model,
			"[real-API fix] failure payload MUST carry the requested model — the real Revenium API rejects model=\"\" with HTTP 400")
		require.Equal(t, MiddlewareSourceGuardrail, payload.MiddlewareSource)
		require.Equal(t, "rate limit exceeded", payload.ErrorReason)
	})

	t.Run("model fallback — ResolvedModelUsed empty, OriginalModelRequested wins", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: ptrString("evt_fb"),
			Error:   &schemas.ErrorField{Message: "boom"},
			ExtraFields: schemas.BifrostErrorExtraFields{
				Provider:               "openai",
				ResolvedModelUsed:      "",
				OriginalModelRequested: "gpt-4o",
			},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.Equal(t, "gpt-4o", payload.Model,
			"[real-API fix] empty ResolvedModelUsed MUST fall back to OriginalModelRequested")
	})

	t.Run("model blank only when genuinely unknowable — both error model fields empty", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: ptrString("evt_pre"),
			Error:   &schemas.ErrorField{Message: "dns failure"},
			ExtraFields: schemas.BifrostErrorExtraFields{
				Provider:               "openai",
				ResolvedModelUsed:      "",
				OriginalModelRequested: "",
			},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.Empty(t, payload.Model,
			"[documented] pre-routing transport error -> model genuinely unknowable -> blank")
	})

	t.Run("EventID nil — fallback to error-no-id sentinel", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: nil,
			Error:   &schemas.ErrorField{Message: "boom"},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.Equal(t, "error-no-id", payload.TransactionID,
			"[METER-02] nil EventID -> sentinel transaction id")
	})

	t.Run("EventID empty string — fallback to error-no-id sentinel", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: ptrString(""),
			Error:   &schemas.ErrorField{Message: "boom"},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.Equal(t, "error-no-id", payload.TransactionID,
			"[METER-02] empty *EventID -> sentinel (empty pointer treated as missing)")
	})

	t.Run("nil bifrostErr — caller skips Send (returns nil)", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		require.Nil(t, BuildFailure(ctx, nil),
			"[METER-02] nil bifrostErr -> nil payload (caller skips Send)")
	})

	t.Run("nil Error field — payload non-nil with empty errorReason + StopReason=ERROR", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: ptrString("evt_1"),
			Error:   nil,
			ExtraFields: schemas.BifrostErrorExtraFields{
				Provider: "openai",
			},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.Equal(t, "ERROR", payload.StopReason,
			"WithError side-effect MUST set StopReason=ERROR even when errorReason==''")
	})

	t.Run("provider sourced from ExtraFields.Provider — bedrock", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: ptrString("evt_b"),
			Error:   &schemas.ErrorField{Message: "throttled"},
			ExtraFields: schemas.BifrostErrorExtraFields{
				Provider: "bedrock",
			},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.Equal(t, "bedrock", payload.Provider,
			"[METER-07 parity on failure path] provider from bifrostErr.ExtraFields.Provider")
	})

	t.Run("Pitfall 13 — MiddlewareSource override", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: ptrString("evt_x"),
			Error:   &schemas.ErrorField{Message: "boom"},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.NotEqual(t, "revenium-go-sdk", payload.MiddlewareSource)
		require.Equal(t, "GUARDRAIL", payload.MiddlewareSource)
	})

	t.Run("METER-02 zero tokens", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		bifrostErr := &schemas.BifrostError{
			EventID: ptrString("evt_z"),
			Error:   &schemas.ErrorField{Message: "boom"},
		}
		payload := BuildFailure(ctx, bifrostErr)
		require.NotNil(t, payload)
		require.EqualValues(t, 0, payload.InputTokenCount)
		require.EqualValues(t, 0, payload.OutputTokenCount)
		require.EqualValues(t, 0, payload.TotalTokenCount)
	})
}
