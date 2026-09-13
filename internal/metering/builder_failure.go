package metering

import (
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	revsdk "github.com/revenium/revenium-go-sdk/core/metering"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/identity"
)

// BuildFailure constructs a *revsdk.MeteringPayload describing a failed LLM
// call (provider error, non-2xx response, transport failure) per METER-02 +
// the D-29 field-mapping truth table.
//
// Returns nil when bifrostErr is nil — PostLLMHook caller (Plan 03-02) MUST
// skip Send on a nil return.
//
// Field mapping (D-29):
//
//   - stop_reason=ERROR — set as a side effect of PayloadBuilder.WithError
//     (see SDK payload.go:140 — WithError writes ErrorReason AND
//     StopReason="ERROR" in one call). Relying on the side effect keeps the
//     fluent chain compact; redundant explicit WithStopReason("ERROR") is
//     omitted.
//   - transaction_id ← bifrostErr.EventID (top-level *string per Phase 2
//     convention) → "error-no-id" sentinel when EventID is nil OR empty
//     pointer. The sentinel is a deliberate analytics signal per CONTEXT.md
//     <specifics> — do NOT substitute revsdk.GenerateTransactionID() here.
//   - provider ← bifrostErr.ExtraFields.Provider (direct field access; the
//     ExtraFields is a VALUE type on *BifrostError, NOT a pointer accessed
//     via GetExtraFields() — see 03-01-SUMMARY.md "Deviations" Rule 1).
//   - model ← bifrostErr.ExtraFields.ResolvedModelUsed, falling back to
//     bifrostErr.ExtraFields.OriginalModelRequested when the resolved value
//     is empty (PopulateExtraFields in core@v1.5.11 sets ResolvedModelUsed =
//     OriginalModelRequested when the resolved model is empty, so resolved is
//     the preferred source and falls back cleanly). Both are plain `string`
//     value fields on the BifrostErrorExtraFields value field — same
//     direct-access pattern as the provider read below; there is NO
//     GetExtraFields() accessor on *BifrostError. Model is left blank ONLY
//     when BOTH are empty, i.e. a genuine pre-routing transport error where
//     the model is unknowable. PYTHON-DELTA/D-29: the real Revenium API
//     rejects model="" with HTTP 400 "model is required and cannot be
//     blank", so sourcing the requested model here is what lets a failed
//     (provider-error) completion meter at all.
//   - tokens are zero (METER-02 explicit contract — failures do not bill).
//   - middleware_source=GUARDRAIL — explicit override after Build() (Pitfall
//     13; SDK default is "revenium-go-sdk").
//   - identity fields populated via direct struct assignment (Delta A) — same
//     block as BuildSuccess; per CONTEXT.md <decisions> Claude's Discretion,
//     three builders × ~12 lines is below the deduplication threshold and
//     inline read is clearer than a shared helper.
//
// PYTHON-DELTA(METER-02): WithError errorReason captures bifrostErr.Error.Message;
// LiteLLM's failure-hook puts the same in `exception_message`. Bifrost's
// transport-error shape gives us the message via the typed ErrorField struct.
func BuildFailure(ctx *schemas.BifrostContext, bifrostErr *schemas.BifrostError) *revsdk.MeteringPayload {
	if bifrostErr == nil {
		return nil
	}

	// Transaction-id fallback chain per CONTEXT.md <specifics> + threat
	// model T-03-01-04: caller-correlated EventID is preferred; the
	// "error-no-id" sentinel is a deliberate analytics signal that lets
	// downstream reporting distinguish untraceable errors.
	txID := "error-no-id"
	if bifrostErr.EventID != nil && *bifrostErr.EventID != "" {
		txID = *bifrostErr.EventID
	}

	// Error message — drives WithError, which sets StopReason="ERROR" as a
	// side effect (see SDK payload.go:140).
	errMsg := ""
	if bifrostErr.Error != nil {
		errMsg = bifrostErr.Error.Message
	}

	// Provider via direct ExtraFields field access. Note: 03-RESEARCH.md +
	// 03-01-PLAN.md text referred to bifrostErr.GetExtraFields() (returning
	// *BifrostErrorExtraFields), but that method does NOT exist on
	// *BifrostError in bifrost/core@v1.5.11 — only *BifrostResponse has the
	// GetExtraFields helper. BifrostError.ExtraFields is a value field of
	// type BifrostErrorExtraFields, so we read .Provider directly.
	provider := string(bifrostErr.ExtraFields.Provider)

	// Model: prefer the resolved model, fall back to the originally-requested
	// model. Blank ONLY when both are empty (genuine pre-routing transport
	// error). The real Revenium API rejects a blank model with HTTP 400, so on
	// a provider error — where the requested model IS known — we MUST emit it.
	model := bifrostErr.ExtraFields.ResolvedModelUsed
	if model == "" {
		model = bifrostErr.ExtraFields.OriginalModelRequested
	}

	// Comma-ok read of the PreLLMHook timestamp with defensive time.Now() fallback.
	start, ok := ctx.Value(ctxkeys.KeyRequestStart).(time.Time)
	if !ok {
		start = time.Now()
	}

	// Build chain: WithError sets StopReason="ERROR" as a side effect — see
	// SDK payload.go:140. METER-02 zero tokens.
	b := revsdk.NewPayload(revsdk.OperationChat, model, provider)
	b = b.WithTiming(start, time.Since(start))
	b = b.WithTokens(0, 0, 0)
	b = b.WithError(errMsg)
	b = b.WithTransactionID(txID)
	payload := b.Build()

	// PITFALL-13: SDK default is "revenium-go-sdk" — override to our locked constant.
	payload.MiddlewareSource = MiddlewareSourceGuardrail

	// Identity direct-struct assignment (Delta A) — same block as BuildSuccess.
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
