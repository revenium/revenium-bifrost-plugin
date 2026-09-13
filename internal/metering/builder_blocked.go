package metering

import (
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	revsdk "github.com/revenium/revenium-go-sdk/core/metering"

	"github.com/revenium/revenium-bifrost-plugin/internal/budget"
	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/identity"
)

// BuildBlocked constructs a *revsdk.MeteringPayload describing a
// budget-blocked (short-circuited) request that NEVER reached a provider, per
// METER-03 + the D-29 field-mapping truth table.
//
// METER-03 SYMMETRY GUARANTEE: BuildBlocked NEVER returns nil — even when
// d is nil OR d.Budget is nil. The PostLLMHook caller (Plan 03-02) MUST
// always Send on a block so the block count is observable platform-side; the
// alternative (skip Send when budget context is missing) would silently drop
// blocked events and regress the contract.
//
// Field mapping (D-29 + CONTEXT.md <specifics>):
//
//   - stop_reason="BUDGET_EXCEEDED" — note: literal string is NOT in the
//     SDK's stop_reason.go constants (verified Research §1); inlined here as
//     a Revenium-specific stop-reason per the truth table.
//   - transaction_id ← revsdk.GenerateTransactionID() (no upstream resp.ID
//     exists; the block fires BEFORE the provider call).
//   - model ← ctx KeyRequestModel; provider ← ctx KeyRequestProvider — both
//     stashed by PreLLMHook from the inbound req.ChatRequest before the
//     budget-check branches. The call never reaches a provider, so this is
//     the REQUESTED model, not a resolved one. Defaults to "" when absent
//     (non-chat request) without breaking the NEVER-nil guarantee. The real
//     Revenium API rejects model="" with HTTP 400, so the block event must
//     carry the requested model.
//   - request_duration=0 — the call never went to a provider.
//   - middleware_source="GUARDRAIL" — explicit override (Pitfall 13).
//   - Attributes.{budget.name, budget.threshold, budget.currentValue,
//     budget.percentUsed} ← d.Budget.* (the WithAttributes channel surfaces
//     which budget tripped without bloating top-level payload fields).
//   - identity fields populated via direct struct assignment (Delta A) —
//     block requests carry caller identity for analytics.
//
// This is the constructor PostLLMHook (Plan 03-02) calls FIRST when it
// detects ctx.Value(ctxkeys.KeyShortCircuited) != nil. Without this check
// PostLLMHook would otherwise call BuildSuccess/BuildFailure and double-count
// the block as a billable LLM call.
//
// Per Warning 8 in 01-03-PLAN.md, this is the ONLY file in this repo that
// imports internal/budget. The emit-on-block flow is orchestrated entirely by
// plugin.go's hook body, not by code in internal/budget reaching across to
// internal/metering. Going the other direction would create a circular
// import the day internal/budget gained an import of internal/metering — the
// grep gate in Plan 03 Task 5 enforces that this never happens.
//
// PACKAGE RULE: internal/metering -> internal/budget is the one permitted
// edge. internal/budget -> internal/metering MUST never exist (would create
// a circular import). See Pattern H grep gate.
func BuildBlocked(ctx *schemas.BifrostContext, d *budget.Decision) *revsdk.MeteringPayload {
	// Transaction id: blocked path has no upstream resp.ID. Use the SDK's
	// UUID-shaped generator (per CONTEXT.md <specifics>).
	txID := revsdk.GenerateTransactionID()

	// Timing: zero-duration window. The call NEVER reached a provider.
	start := time.Now()

	// Model + provider source from the pre-routing inbound request via ctx
	// (stashed by PreLLMHook under KeyRequestModel/KeyRequestProvider). The
	// block fires BEFORE the call reaches a provider, so this is the REQUESTED
	// model, not a resolved one. Comma-ok typed asserts default to "" when the
	// key is absent (defensive / non-chat request) — the NEVER-nil symmetry
	// guarantee is preserved. The real Revenium API rejects model="" with HTTP
	// 400, so emitting the requested model here is what lets a blocked event
	// meter.
	model, _ := ctx.Value(ctxkeys.KeyRequestModel).(string)
	provider, _ := ctx.Value(ctxkeys.KeyRequestProvider).(string)

	// BUDGET_EXCEEDED: literal NOT in SDK stop_reason.go constants — Revenium-specific
	// per D-29.
	b := revsdk.NewPayload(revsdk.OperationChat, model, provider)
	b = b.WithTiming(start, 0)
	b = b.WithTokens(0, 0, 0)
	b = b.WithStopReason("BUDGET_EXCEEDED")
	b = b.WithTransactionID(txID)
	payload := b.Build()

	// PITFALL-13: SDK default is "revenium-go-sdk" — override to our locked constant.
	payload.MiddlewareSource = MiddlewareSourceGuardrail

	// Budget attribute attachment via direct field assignment on
	// MeteringPayload.Attributes (the field is exported per SDK
	// payload.go; avoids the double-Build pattern shown in 03-PATTERNS.md
	// alternative). Conditional on d + d.Budget non-nil — the
	// symmetry-guarantee path emits a BUDGET_EXCEEDED event with empty
	// Attributes when budget context is unavailable.
	if d != nil && d.Budget != nil {
		payload.Attributes = map[string]interface{}{
			"budget.name":         d.Budget.Name,
			"budget.threshold":    d.Budget.Threshold,
			"budget.currentValue": d.Budget.CurrentValue,
			"budget.percentUsed":  d.Budget.PercentUsed,
		}
	}

	// Identity direct-struct assignment (Delta A) — identical block to
	// BuildSuccess + BuildFailure. Caller identity propagates on block
	// requests for analytics.
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
