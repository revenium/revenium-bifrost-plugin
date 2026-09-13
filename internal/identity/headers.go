package identity

import (
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
)

// Subscriber bundles the four x-revenium-subscriber-* and credential-related
// fields. CredentialName/CredentialValue fall back to Email when the dedicated
// credential headers are absent — matches the Python middleware's
// _resolve_credentials helper. All fields are strings; no validation in stubs.
type Subscriber struct {
	ID              string
	Email           string
	CredentialName  string
	CredentialValue string
}

// Organization bundles x-revenium-organization-name + x-revenium-organization-id.
// Name takes precedence in the metering payload when both are present (matches
// _extract_organization_name precedence).
type Organization struct {
	Name string
	ID   string
}

// Product bundles x-revenium-product-name + x-revenium-product-id. Name takes
// precedence in the metering payload when both are present (matches
// _extract_product_name precedence).
type Product struct {
	Name string
	ID   string
}

// TraceContext bundles the five trace/agent/subscription/quality headers that
// flow through the metering payload's trace_* and agent fields.
// ResponseQualityScore is the raw string (Phase 3 parses to *float64 for the
// SDK builder per METER-01).
type TraceContext struct {
	TraceID              string
	TaskType             string
	Agent                string
	SubscriptionID       string
	ResponseQualityScore string
}

// ExtractKeyHash returns the caller-supplied x-revenium-key-hash header value
// from the Bifrost request context. The HTTPTransportPreHook (Plan 02-02)
// writes the canonicalized value via ctxkeys.KeyHeaderKeyHash; this reader is
// the single sanctioned consumer for the budget-check path (BUDGET-01) and
// the metering payload (Phase 3).
//
// Returns "" when ctx is nil, the key is absent, the key holds an empty
// string, or the key holds a non-string value — comma-ok cast defends
// against the wrong-type-write scenario per Pitfall 7 (forcetypeassert).
func ExtractKeyHash(ctx *schemas.BifrostContext) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(ctxkeys.KeyHeaderKeyHash).(string)
	return v
}

// ExtractSubscriber returns a populated *Subscriber from the
// x-revenium-subscriber-id + x-revenium-subscriber-email ctxkeys. When Email
// is non-empty AND no dedicated credential ctxkey is present (per CONTEXT.md
// `<deferred>` "Credential header source resolution", Bifrost v1.5.x exposes
// no x-revenium-credential-* headers in v1), CredentialName and
// CredentialValue both fall back to Email — matches the Python middleware's
// _resolve_credentials helper. Per IDENTITY-02.
//
// Returns nil when ctx is nil OR when all four resulting fields are empty —
// downstream metering payload can omit the subscriber field cleanly.
func ExtractSubscriber(ctx *schemas.BifrostContext) *Subscriber {
	if ctx == nil {
		return nil
	}
	id, _ := ctx.Value(ctxkeys.KeyHeaderSubscriberID).(string)
	email, _ := ctx.Value(ctxkeys.KeyHeaderSubscriberEmail).(string)

	// Email-as-credential fallback (Python _resolve_credentials parity).
	// When dedicated credential headers exist in a future Bifrost contract,
	// replace this branch with the corresponding ctxkey reads.
	credName, credValue := "", ""
	if email != "" {
		credName = email
		credValue = email
	}

	if id == "" && email == "" && credName == "" && credValue == "" {
		return nil
	}
	return &Subscriber{
		ID:              id,
		Email:           email,
		CredentialName:  credName,
		CredentialValue: credValue,
	}
}

// ExtractOrganization returns a populated *Organization from the
// x-revenium-organization-name and x-revenium-organization-id ctxkeys. Per
// IDENTITY-03, both fields are surfaced verbatim; the name→id precedence
// rule from Python _extract_organization_name is applied by the
// metering-payload consumer (Phase 3), NOT here — keeping the Extract layer
// dumb-read keeps the reader nil-safe and obvious.
//
// Returns nil when ctx is nil OR when both fields end up empty.
func ExtractOrganization(ctx *schemas.BifrostContext) *Organization {
	if ctx == nil {
		return nil
	}
	name, _ := ctx.Value(ctxkeys.KeyHeaderOrgName).(string)
	id, _ := ctx.Value(ctxkeys.KeyHeaderOrgID).(string)
	if name == "" && id == "" {
		return nil
	}
	return &Organization{Name: name, ID: id}
}

// ExtractProduct returns a populated *Product from the
// x-revenium-product-name and x-revenium-product-id ctxkeys. Per IDENTITY-04,
// both fields are surfaced verbatim; the name→id precedence rule from Python
// _extract_product_name is applied by the metering-payload consumer (Phase
// 3), NOT here.
//
// Returns nil when ctx is nil OR when both fields end up empty.
func ExtractProduct(ctx *schemas.BifrostContext) *Product {
	if ctx == nil {
		return nil
	}
	name, _ := ctx.Value(ctxkeys.KeyHeaderProductName).(string)
	id, _ := ctx.Value(ctxkeys.KeyHeaderProductID).(string)
	if name == "" && id == "" {
		return nil
	}
	return &Product{Name: name, ID: id}
}

// ExtractTraceContext returns a populated *TraceContext from the five trace
// ctxkeys (x-revenium-trace-id, x-revenium-task-type, x-revenium-agent,
// x-revenium-subscription-id, x-revenium-response-quality-score). Per
// IDENTITY-05, missing values are surfaced as empty strings inside the
// returned struct (NOT nil); the metering-payload builder is the layer that
// maps empty-string → omitted-field.
//
// Returns nil when ctx is nil OR when all five fields end up empty.
func ExtractTraceContext(ctx *schemas.BifrostContext) *TraceContext {
	if ctx == nil {
		return nil
	}
	traceID, _ := ctx.Value(ctxkeys.KeyHeaderTraceID).(string)
	taskType, _ := ctx.Value(ctxkeys.KeyHeaderTaskType).(string)
	agent, _ := ctx.Value(ctxkeys.KeyHeaderAgent).(string)
	subID, _ := ctx.Value(ctxkeys.KeyHeaderSubscriptionID).(string)
	qScore, _ := ctx.Value(ctxkeys.KeyHeaderResponseQualityScore).(string)

	if traceID == "" && taskType == "" && agent == "" && subID == "" && qScore == "" {
		return nil
	}
	return &TraceContext{
		TraceID:              traceID,
		TaskType:             taskType,
		Agent:                agent,
		SubscriptionID:       subID,
		ResponseQualityScore: qScore,
	}
}
