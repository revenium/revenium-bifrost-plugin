package metering

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/budget"
	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
)

// TestBuildBlocked locks the METER-03 BUDGET_EXCEEDED path and the seven
// subtests defined in 03-01-PLAN.md <behavior>.
func TestBuildBlocked(t *testing.T) {
	t.Run("happy block path — populated budget + headers", func(t *testing.T) {
		ctx := newTestCtx()
		setAllHeaders(ctx)
		ctx.SetValue(ctxkeys.KeyRequestModel, "gpt-4o-mini")
		ctx.SetValue(ctxkeys.KeyRequestProvider, "openai")

		d := &budget.Decision{
			Allowed:       false,
			ExceededCount: 1,
			Budget: &budget.Budget{
				Name:         "daily-spend",
				Threshold:    100.0,
				CurrentValue: 105.5,
				PercentUsed:  105.5,
				Risk:         "EXCEEDED",
			},
		}
		payload := BuildBlocked(ctx, d)
		require.NotNil(t, payload, "[METER-03] block path returns non-nil payload")

		require.Equal(t, "BUDGET_EXCEEDED", payload.StopReason)
		require.EqualValues(t, 0, payload.InputTokenCount)
		require.EqualValues(t, 0, payload.OutputTokenCount)
		require.EqualValues(t, 0, payload.TotalTokenCount)
		require.EqualValues(t, 0, payload.RequestDuration,
			"[METER-03] request_duration MUST be 0 — call never reached provider")
		require.Equal(t, MiddlewareSourceGuardrail, payload.MiddlewareSource)
		require.Equal(t, "gpt-4o-mini", payload.Model,
			"[real-API fix] block event MUST carry the requested model from ctx — real API rejects model=\"\"")
		require.Equal(t, "openai", payload.Provider,
			"[METER-07 parity on block path] provider from the inbound request via ctx")

		require.NotNil(t, payload.Attributes)
		require.Equal(t, "daily-spend", payload.Attributes["budget.name"])
		require.Equal(t, 100.0, payload.Attributes["budget.threshold"])
		require.Equal(t, 105.5, payload.Attributes["budget.currentValue"])
		require.Equal(t, 105.5, payload.Attributes["budget.percentUsed"])

		require.NotNil(t, payload.Subscriber)
		require.Equal(t, "sub-42", payload.Subscriber["id"])
	})

	t.Run("no KeyRequestModel set — empty model, NEVER-nil payload preserved", func(t *testing.T) {
		ctx := newTestCtx()
		d := &budget.Decision{Allowed: false, Budget: &budget.Budget{Name: "no-model"}}
		payload := BuildBlocked(ctx, d)
		require.NotNil(t, payload,
			"[METER-03] missing request-model key MUST NOT break the symmetry NEVER-nil guarantee")
		require.Empty(t, payload.Model,
			"defensive: no KeyRequestModel in ctx -> blank model, no panic")
		require.Equal(t, "BUDGET_EXCEEDED", payload.StopReason)
	})

	t.Run("transaction id — non-empty UUID-shaped string", func(t *testing.T) {
		ctx := newTestCtx()
		d := &budget.Decision{Allowed: false, Budget: &budget.Budget{Name: "x"}}
		payload := BuildBlocked(ctx, d)
		require.NotNil(t, payload)
		require.NotEmpty(t, payload.TransactionID,
			"[METER-03] blocked payload must carry a non-empty transaction id")
		require.Len(t, payload.TransactionID, 36,
			"[METER-03] GenerateTransactionID returns UUID v4 (36 chars incl dashes)")
	})

	t.Run("nil Decision — METER-03 symmetry guarantee: still emit BUDGET_EXCEEDED", func(t *testing.T) {
		ctx := newTestCtx()
		payload := BuildBlocked(ctx, nil)
		require.NotNil(t, payload,
			"[METER-03] nil Decision MUST still emit a BUDGET_EXCEEDED event (no skip on block)")
		require.Equal(t, "BUDGET_EXCEEDED", payload.StopReason)
		require.True(t, len(payload.Attributes) == 0,
			"nil Decision -> empty/nil Attributes")
	})

	t.Run("nil Decision.Budget — non-nil payload + empty Attributes", func(t *testing.T) {
		ctx := newTestCtx()
		d := &budget.Decision{Allowed: false, Budget: nil}
		payload := BuildBlocked(ctx, d)
		require.NotNil(t, payload)
		require.Equal(t, "BUDGET_EXCEEDED", payload.StopReason)
		require.True(t, len(payload.Attributes) == 0)
	})

	t.Run("identity present on block — caller identity propagates", func(t *testing.T) {
		ctx := newTestCtx()
		setAllHeaders(ctx)
		d := &budget.Decision{Allowed: false, Budget: &budget.Budget{Name: "y"}}
		payload := BuildBlocked(ctx, d)
		require.NotNil(t, payload)
		require.NotNil(t, payload.Subscriber)
		require.Equal(t, "sub-42", payload.Subscriber["id"])
		require.Equal(t, "Acme Corp", payload.OrganizationName)
		require.Equal(t, "Widgets", payload.ProductName)
		require.Equal(t, "trace-xyz", payload.TraceID)
		require.Equal(t, "chat", payload.TaskType)
		require.Equal(t, "agent-007", payload.Agent)
		require.Equal(t, "subn-9", payload.SubscriptionID)
	})

	t.Run("Pitfall 13 — MiddlewareSource override", func(t *testing.T) {
		ctx := newTestCtx()
		d := &budget.Decision{Allowed: false, Budget: &budget.Budget{Name: "z"}}
		payload := BuildBlocked(ctx, d)
		require.NotNil(t, payload)
		require.NotEqual(t, "revenium-go-sdk", payload.MiddlewareSource)
		require.Equal(t, "GUARDRAIL", payload.MiddlewareSource)
	})

	t.Run("request_duration == 0", func(t *testing.T) {
		ctx := newTestCtx()
		d := &budget.Decision{Allowed: false, Budget: &budget.Budget{Name: "w"}}
		payload := BuildBlocked(ctx, d)
		require.NotNil(t, payload)
		require.EqualValues(t, 0, payload.RequestDuration,
			"[METER-03] block path never went to provider — RequestDuration MUST be 0")
	})
}
