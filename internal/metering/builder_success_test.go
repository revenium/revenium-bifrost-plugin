package metering

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
)

// newTestCtx constructs a real *BifrostContext usable in unit tests. Mirrors
// the in-repo pattern from internal/identity/headers_test.go — Bifrost's
// NewBifrostContext is the only constructor that initializes the userValues
// map, so ctx.SetValue / ctx.Value round-trip works in test code.
func newTestCtx() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

// setHeaders sets all twelve KeyHeader* ctxkeys to non-empty values so the
// identity Extract* helpers all return populated structs. Used by happy-path
// subtests to assert identity propagation through the builder.
func setAllHeaders(ctx *schemas.BifrostContext) {
	ctx.SetValue(ctxkeys.KeyHeaderKeyHash, "sk-abc12345")
	ctx.SetValue(ctxkeys.KeyHeaderSubscriberID, "sub-42")
	ctx.SetValue(ctxkeys.KeyHeaderSubscriberEmail, "a@b.com")
	ctx.SetValue(ctxkeys.KeyHeaderOrgName, "Acme Corp")
	ctx.SetValue(ctxkeys.KeyHeaderOrgID, "org-1")
	ctx.SetValue(ctxkeys.KeyHeaderProductName, "Widgets")
	ctx.SetValue(ctxkeys.KeyHeaderProductID, "prod-1")
	ctx.SetValue(ctxkeys.KeyHeaderTraceID, "trace-xyz")
	ctx.SetValue(ctxkeys.KeyHeaderTaskType, "chat")
	ctx.SetValue(ctxkeys.KeyHeaderAgent, "agent-007")
	ctx.SetValue(ctxkeys.KeyHeaderSubscriptionID, "subn-9")
	ctx.SetValue(ctxkeys.KeyHeaderResponseQualityScore, "0.95")
}

// chatResp is a constructor helper for a populated *BifrostResponse with a
// ChatResponse variant (Delta B — discriminated union).
func chatResp(id, model string, provider schemas.ModelProvider, prompt, completion, total int) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			ID:    id,
			Model: model,
			Usage: &schemas.BifrostLLMUsage{
				PromptTokens:     prompt,
				CompletionTokens: completion,
				TotalTokens:      total,
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				Provider: provider,
			},
		},
	}
}

// TestBuildSuccess locks the METER-01 + METER-07 happy path and the seven
// defensive subtests defined in 03-01-PLAN.md <behavior>.
func TestBuildSuccess(t *testing.T) {
	t.Run("happy path — all headers + populated chat response", func(t *testing.T) {
		ctx := newTestCtx()
		setAllHeaders(ctx)
		start := time.Now().Add(-1500 * time.Millisecond)
		ctx.SetValue(ctxkeys.KeyRequestStart, start)

		resp := chatResp("resp_abc", "gpt-4o", "openai", 10, 20, 30)
		payload := BuildSuccess(ctx, resp)
		require.NotNil(t, payload, "[METER-01] happy path must return non-nil payload")

		require.Equal(t, "gpt-4o", payload.Model)
		require.Equal(t, "openai", payload.Provider)
		require.Equal(t, "openai", payload.ModelSource)
		require.Equal(t, "END", payload.StopReason)
		require.Equal(t, MiddlewareSourceGuardrail, payload.MiddlewareSource)
		require.Equal(t, "resp_abc", payload.TransactionID)
		require.Equal(t, "CHAT", payload.OperationType)
		require.EqualValues(t, 10, payload.InputTokenCount)
		require.EqualValues(t, 20, payload.OutputTokenCount)
		require.EqualValues(t, 30, payload.TotalTokenCount)
		require.GreaterOrEqual(t, payload.RequestDuration, int64(1000),
			"[METER-01] duration must reflect KeyRequestStart -> now")
		require.NotNil(t, payload.Subscriber)
		require.Equal(t, "sub-42", payload.Subscriber["id"])
		require.Equal(t, "a@b.com", payload.Subscriber["email"])
		require.Equal(t, "Acme Corp", payload.OrganizationName)
		require.Equal(t, "org-1", payload.OrganizationID)
		require.Equal(t, "Widgets", payload.ProductName)
		require.Equal(t, "prod-1", payload.ProductID)
		require.Equal(t, "trace-xyz", payload.TraceID)
		require.Equal(t, "chat", payload.TaskType)
		require.Equal(t, "agent-007", payload.Agent)
		require.Equal(t, "subn-9", payload.SubscriptionID)
	})

	t.Run("nil response — caller skips Send (returns nil)", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		payload := BuildSuccess(ctx, nil)
		require.Nil(t, payload, "[METER-01] nil resp -> nil payload (caller skips Send)")
	})

	t.Run("nil ChatResponse — non-chat response variant returns nil", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		payload := BuildSuccess(ctx, &schemas.BifrostResponse{})
		require.Nil(t, payload, "[Delta B] non-chat discriminated union returns nil")
	})

	t.Run("no identity headers — payload non-nil with empty identity fields", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		resp := chatResp("resp_x", "gpt-4o", "openai", 1, 2, 3)
		payload := BuildSuccess(ctx, resp)
		require.NotNil(t, payload)
		require.Nil(t, payload.Subscriber, "no headers -> Subscriber map left nil")
		require.Equal(t, "", payload.OrganizationName)
		require.Equal(t, "", payload.ProductName)
		require.Equal(t, "", payload.TraceID)
	})

	t.Run("missing KeyRequestStart — defensive fallback, no panic", func(t *testing.T) {
		ctx := newTestCtx()
		// No KeyRequestStart set.
		resp := chatResp("resp_x", "gpt-4o", "openai", 1, 2, 3)
		require.NotPanics(t, func() {
			payload := BuildSuccess(ctx, resp)
			require.NotNil(t, payload, "defensive: builder must NOT panic on missing KeyRequestStart")
		})
	})

	t.Run("METER-07 provider sourcing — anthropic, NOT LITELLM", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		resp := chatResp("resp_a", "claude-3-5-sonnet", "anthropic", 5, 5, 10)
		payload := BuildSuccess(ctx, resp)
		require.NotNil(t, payload)
		require.Equal(t, "anthropic", payload.Provider, "[METER-07] provider from resp.ChatResponse.ExtraFields.Provider")
		require.Equal(t, "anthropic", payload.ModelSource, "[METER-07] model_source mirrors provider")
		require.NotEqual(t, "LITELLM", payload.Provider, "[METER-07] never hardcoded LITELLM")
	})

	t.Run("Pitfall 13 — MiddlewareSource override (NOT SDK default)", func(t *testing.T) {
		ctx := newTestCtx()
		ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
		resp := chatResp("resp_x", "gpt-4o", "openai", 1, 2, 3)
		payload := BuildSuccess(ctx, resp)
		require.NotNil(t, payload)
		require.NotEqual(t, "revenium-go-sdk", payload.MiddlewareSource,
			"[Pitfall 13] SDK default must be overridden")
		require.Equal(t, "GUARDRAIL", payload.MiddlewareSource,
			"[Pitfall 13] MiddlewareSource MUST equal locked constant")
	})
}
