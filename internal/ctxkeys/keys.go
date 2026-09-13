// Package ctxkeys centralizes the *schemas.BifrostContext keys used by the
// Revenium Bifrost plugin. Every key value is prefixed with "revenium-bifrost."
// to mitigate Pitfall 17 (multi-plugin keyspace collisions): Bifrost's
// BifrostContext.SetValue/Value uses a flat keyspace shared across every
// loaded plugin, so unprefixed names like "trace-id" or "key-hash" can be
// silently clobbered by another plugin that uses the same string.
//
// All Revenium keys MUST be declared as exported constants in this file. No
// other package in this module is permitted to construct a
// schemas.BifrostContextKey from a string literal — that would defeat the
// single-source-of-truth contract this package provides.
package ctxkeys

import "github.com/maximhq/bifrost/core/schemas"

// All Revenium plugin context keys are prefixed with "revenium-bifrost." to
// avoid collision with other plugins' keys (e.g., bf-governance-*). This
// namespace is documented in PROJECT.md as reserved for this plugin.
const (
	// KeyHash is the legacy single-purpose context key for the virtual-key hash.
	// SUPERSEDED in Phase 2 by KeyHeaderKeyHash (set by HTTPTransportPreHook from the
	// x-revenium-key-hash header). Preserved for backward compatibility with Phase 1
	// pluginsanity tests; new code should read KeyHeaderKeyHash.
	KeyHash schemas.BifrostContextKey = "revenium-bifrost.key-hash"

	// KeyRequestStart carries the time.Time when PreLLMHook fired. Set by
	// PreLLMHook; read by PostLLMHook (for non-streaming response timing) and
	// by HTTPTransportStreamChunkHook (for the TTFT — time-to-first-token —
	// calculation on the first non-terminal chunk).
	KeyRequestStart schemas.BifrostContextKey = "revenium-bifrost.request-start"

	// KeyStreamState carries the *streaming.State accumulator (token counts,
	// TTFT, first-chunk timestamp). Set by PreLLMHook when the inbound
	// request is detected as streaming; mutated by
	// HTTPTransportStreamChunkHook on every chunk; finalized in the
	// terminal-chunk branch of HTTPTransportStreamChunkHook — NOT in
	// PostLLMHook, per research Correction 2 (chunk hook fires AFTER
	// PostLLMHook, so PostLLMHook does not see final usage counts).
	KeyStreamState schemas.BifrostContextKey = "revenium-bifrost.stream-state"

	// KeyRequestType carries the schemas.RequestType of the inbound request.
	// Written by PreLLMHook (Plan 03-02 Task 1 — 1-line addition next to the
	// existing KeyRequestStart write); read by PostLLMHook (Plan 03-02
	// Task 2) for the streaming-no-op dispatch branch (METER-05).
	//
	// Rationale per 03-RESEARCH.md §3 Delta D: PostLLMHook's signature
	// `PostLLMHook(ctx, resp, bifrostErr)` does NOT receive *BifrostRequest,
	// so it cannot read req.RequestType directly. Pre-stashing the
	// RequestType in ctx during PreLLMHook is the most robust detection
	// path — it works even when resp AND bifrostErr are both nil-ish and
	// is independent of resp.GetExtraFields() shape drift.
	//
	// METER-05 contract: when ctx.Value(KeyRequestType) ==
	// schemas.ChatCompletionStreamRequest, PostLLMHook MUST emit ZERO
	// metering events — HTTPTransportStreamChunkHook (Plan 03-03) fires
	// AFTER PostLLMHook on streamed responses and is the sole roll-up
	// emitter for streaming completions.
	KeyRequestType schemas.BifrostContextKey = "revenium-bifrost.request-type"

	// KeyRequestModel carries the inbound request's model name
	// (req.ChatRequest.Model). Written by PreLLMHook (before the budget-check
	// branches so it is set even on the short-circuit/block path); read by
	// metering.BuildBlocked so the BUDGET_EXCEEDED symmetry event carries the
	// requested model. The block fires pre-routing, so this is the requested
	// model, not a provider-resolved one. The real Revenium API rejects a
	// blank model with HTTP 400, so the blocked event must carry it too.
	KeyRequestModel schemas.BifrostContextKey = "revenium-bifrost.request-model"

	// KeyRequestProvider carries the inbound request's provider attribution
	// (req.ChatRequest.Provider). Written by PreLLMHook alongside
	// KeyRequestModel; read by metering.BuildBlocked for the BUDGET_EXCEEDED
	// symmetry event's provider field.
	KeyRequestProvider schemas.BifrostContextKey = "revenium-bifrost.request-provider"

	// KeyDryRunDecision carries a *budget.Decision when REVENIUM_DRY_RUN=true
	// and a budget block WOULD have happened (but the request was allowed
	// through). Set by PreLLMHook in dry-run mode; read by PostLLMHook so it
	// can attach a "would_have_blocked" flag to the metering payload.
	KeyDryRunDecision schemas.BifrostContextKey = "revenium-bifrost.dry-run-decision"

	// KeyShortCircuited carries a *budget.Decision when PreLLMHook actually
	// short-circuited (budget exceeded, request blocked). PostLLMHook MUST
	// check this key FIRST to avoid double-counting blocks as billable LLM
	// calls — per research Correction 1, Bifrost's symmetry guarantee fires
	// PostLLMHook even after a PreLLMHook short-circuit (METER-03).
	KeyShortCircuited schemas.BifrostContextKey = "revenium-bifrost.short-circuited"

	// --- Phase 2 D-19: one ctxkey per identity-bearing x-revenium-* header ---
	//
	// Every KeyHeader* below is the ctx-side endpoint for exactly one header
	// listed in CLAUDE.md "Configuration surface" / PROJECT.md "x-revenium-*"
	// contract. All KeyHeader* keys are written exclusively by
	// HTTPTransportPreHook (Plan 02-02) after a case-insensitive header
	// lookup, and read exclusively by the matching internal/identity.Extract*
	// helper (Plan 02-01). The Pitfall 17 namespace contract is extended one
	// level: every value starts with "revenium-bifrost.header." so a future
	// non-header ctxkey (e.g., a derived value) cannot accidentally collide
	// with these reader keys.

	// KeyHeaderKeyHash carries the caller-supplied x-revenium-key-hash header
	// value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractKeyHash (Plan 02-01). Traces BUDGET-01 + IDENTITY-01.
	KeyHeaderKeyHash schemas.BifrostContextKey = "revenium-bifrost.header.key-hash"

	// KeyHeaderSubscriberID carries the caller-supplied x-revenium-subscriber-id
	// header value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractSubscriber (Plan 02-01). Traces IDENTITY-02.
	KeyHeaderSubscriberID schemas.BifrostContextKey = "revenium-bifrost.header.subscriber-id"

	// KeyHeaderSubscriberEmail carries the caller-supplied x-revenium-subscriber-email
	// header value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractSubscriber (Plan 02-01) — also drives the email-as-credential
	// fallback per Python _resolve_credentials. Traces IDENTITY-02.
	KeyHeaderSubscriberEmail schemas.BifrostContextKey = "revenium-bifrost.header.subscriber-email"

	// KeyHeaderOrgName carries the caller-supplied x-revenium-organization-name
	// header value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractOrganization (Plan 02-01). The name→id precedence (Python
	// _extract_organization_name) is applied by the metering-payload consumer,
	// NOT here. Traces IDENTITY-03.
	KeyHeaderOrgName schemas.BifrostContextKey = "revenium-bifrost.header.organization-name"

	// KeyHeaderOrgID carries the caller-supplied x-revenium-organization-id
	// header value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractOrganization (Plan 02-01). Traces IDENTITY-03.
	KeyHeaderOrgID schemas.BifrostContextKey = "revenium-bifrost.header.organization-id"

	// KeyHeaderProductName carries the caller-supplied x-revenium-product-name
	// header value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractProduct (Plan 02-01). The name→id precedence (Python
	// _extract_product_name) is applied by the metering-payload consumer, NOT
	// here. Traces IDENTITY-04.
	KeyHeaderProductName schemas.BifrostContextKey = "revenium-bifrost.header.product-name"

	// KeyHeaderProductID carries the caller-supplied x-revenium-product-id
	// header value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractProduct (Plan 02-01). Traces IDENTITY-04.
	KeyHeaderProductID schemas.BifrostContextKey = "revenium-bifrost.header.product-id"

	// KeyHeaderTraceID carries the caller-supplied x-revenium-trace-id header
	// value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractTraceContext (Plan 02-01). Traces IDENTITY-05.
	KeyHeaderTraceID schemas.BifrostContextKey = "revenium-bifrost.header.trace-id"

	// KeyHeaderTaskType carries the caller-supplied x-revenium-task-type header
	// value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractTraceContext (Plan 02-01). Traces IDENTITY-05.
	KeyHeaderTaskType schemas.BifrostContextKey = "revenium-bifrost.header.task-type"

	// KeyHeaderAgent carries the caller-supplied x-revenium-agent header value.
	// Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractTraceContext (Plan 02-01). Traces IDENTITY-05.
	KeyHeaderAgent schemas.BifrostContextKey = "revenium-bifrost.header.agent"

	// KeyHeaderSubscriptionID carries the caller-supplied x-revenium-subscription-id
	// header value. Written by HTTPTransportPreHook (Plan 02-02); read by
	// identity.ExtractTraceContext (Plan 02-01). Traces IDENTITY-05.
	KeyHeaderSubscriptionID schemas.BifrostContextKey = "revenium-bifrost.header.subscription-id"

	// KeyHeaderResponseQualityScore carries the caller-supplied
	// x-revenium-response-quality-score header value. Written by
	// HTTPTransportPreHook (Plan 02-02); read by identity.ExtractTraceContext
	// (Plan 02-01). Traces IDENTITY-05.
	KeyHeaderResponseQualityScore schemas.BifrostContextKey = "revenium-bifrost.header.response-quality-score"
)
