package metering

import (
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	revsdk "github.com/revenium/revenium-go-sdk/core/metering"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/identity"
)

// BuildSuccess constructs a *revsdk.MeteringPayload describing a successful
// (non-error, non-blocked, non-streaming) LLM completion per METER-01 + METER-07
// + the D-29 field-mapping truth table.
//
// Returns nil when resp is nil OR resp.ChatResponse is nil (the
// discriminated-union variant indicates a non-chat response category — Delta B
// in 03-RESEARCH.md). PostLLMHook caller (Plan 03-02) MUST skip Send on a nil
// return.
//
// Field mapping (D-29):
//
//   - stop_reason=END (set on the PayloadBuilder + locked by the SDK default)
//   - provider ← resp.ChatResponse.ExtraFields.Provider  (METER-07: NOT
//     hardcoded "LITELLM" as the LiteLLM Python guardrail does — Bifrost-native
//     provider attribution flows through verbatim).
//   - model_source ← provider (same as Python parity: "model_source" tracks the
//     provider that served the call).
//   - middleware_source=GUARDRAIL — explicitly overridden after Build() because
//     the SDK's NewPayload defaults this to "revenium-go-sdk" (Pitfall 13).
//   - token counts ← chat.Usage.{PromptTokens, CompletionTokens, TotalTokens}
//     (Delta B — Usage is *BifrostLLMUsage; nil-defended).
//   - timing ← ctxkeys.KeyRequestStart (set by PreLLMHook) → time.Since(start)
//     for request_duration; defensive fallback to time.Now() when the key is
//     absent so the builder never panics.
//   - transaction_id ← chat.ID (when non-empty; SDK default is a fresh UUID).
//   - identity fields (Subscriber map, Organization*, Product*, Trace*) are
//     populated via direct struct assignment after Build() — Delta A: the SDK
//     has no fluent WithSubscriber*/WithOrganization*/WithTraceID setters.
//
// PYTHON-DELTA(METER-NN): mediation_latency is left zero — Bifrost has no
// analog to LiteLLM's _hidden_params.litellm_overhead_time_ms (deferred).
// PYTHON-DELTA(METER-NN): ResponseQualityScore *float64 conversion deferred to
// Phase 4 fixture-driven population — the identity reader surfaces the raw
// string, and the SDK's WithResponseQualityScore expects *float64 input we do
// not synthesize here.
func BuildSuccess(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse) *revsdk.MeteringPayload {
	// Delta B — BifrostResponse is a discriminated union of ~40 pointer
	// variants. Only the ChatResponse branch is in scope for Phase 3.
	if resp == nil || resp.ChatResponse == nil {
		return nil
	}
	chat := resp.ChatResponse

	// Comma-ok read of the PreLLMHook timestamp. Defensive: when the key
	// is absent (should never happen — PreLLMHook stamps it unconditionally
	// per ctxkeys.KeyRequestStart contract), fall back to time.Now() so
	// request_duration becomes ~0 but the builder does NOT panic.
	start, ok := ctx.Value(ctxkeys.KeyRequestStart).(time.Time)
	if !ok {
		start = time.Now()
	}
	duration := time.Since(start)

	// METER-07: provider attribution from the response's ExtraFields — never
	// the literal "LITELLM" the Python guardrail emits.
	provider := string(chat.ExtraFields.Provider)

	// Token harvest (Delta B — Usage is *BifrostLLMUsage; default to zeros on nil).
	var inputTokens, outputTokens, totalTokens int64
	if u := chat.Usage; u != nil {
		inputTokens = int64(u.PromptTokens)
		outputTokens = int64(u.CompletionTokens)
		totalTokens = int64(u.TotalTokens)
	}

	payload := revsdk.NewPayload(revsdk.OperationChat, chat.Model, provider).
		WithTiming(start, duration).
		WithTokens(inputTokens, outputTokens, totalTokens).
		WithStopReason("END").
		WithModelSource(provider).
		WithTransactionID(chat.ID).
		Build()

	// PITFALL-13: SDK default is "revenium-go-sdk" — override to our locked constant.
	payload.MiddlewareSource = MiddlewareSourceGuardrail

	// Delta A — identity direct-struct assignment (no fluent setters exist).
	if sub := identity.ExtractSubscriber(ctx); sub != nil {
		payload.Subscriber = map[string]interface{}{
			"id":              sub.ID,
			"email":           sub.Email,
			"credentialName":  sub.CredentialName,
			"credentialValue": sub.CredentialValue,
		}
	}
	if org := identity.ExtractOrganization(ctx); org != nil {
		payload.OrganizationName = org.Name
		payload.OrganizationID = org.ID
	}
	if prod := identity.ExtractProduct(ctx); prod != nil {
		payload.ProductName = prod.Name
		payload.ProductID = prod.ID
	}
	if trace := identity.ExtractTraceContext(ctx); trace != nil {
		payload.TraceID = trace.TraceID
		payload.TaskType = trace.TaskType
		payload.Agent = trace.Agent
		payload.SubscriptionID = trace.SubscriptionID
	}

	return payload
}
