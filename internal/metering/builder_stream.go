package metering

import (
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	revsdk "github.com/revenium/revenium-go-sdk/core/metering"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/identity"
	"github.com/revenium/revenium-bifrost-plugin/internal/streaming"
)

// BuildStream constructs a single roll-up MeteringPayload at the end of a
// streamed completion per METER-04 + METER-07 + D-29 field-mapping truth
// table.
//
// Emitted from the terminal-chunk branch of HTTPTransportStreamChunkHook, NOT
// from PostLLMHook. Bifrost fires StreamChunkHook AFTER PostLLMHook on
// streamed responses (Phase 1 spike D-15.2), so emitting from PostLLMHook
// would always record TTFT=0 and total_tokens=0.
//
// Returns nil when state is nil — caller (HTTPTransportStreamChunkHook) MUST
// skip Send on a nil return.
//
// Field mapping (D-29):
//
//   - is_streamed=true — set via WithStreaming.
//   - time_to_first_token ← FirstChunkAt - KeyRequestStart (milliseconds);
//     defensive 0 when FirstChunkAt is zero (no chunks observed before
//     terminal-detect — should not happen in practice but locks the contract
//     to never emit a negative TTFT).
//   - completion_start_time ← &state.Metrics.FirstChunkAt — overrides the
//     WithTiming-default CompletionStartTime per Delta 2 (03-RESEARCH.md §1).
//     WithStreaming MUST be called AFTER WithTiming for the override to win.
//   - token counts ← state.Finalize() (last-wins harvest from OnChunk).
//   - stop_reason=END — successful streaming completion. Error-terminal
//     streams flow through this same builder; the FinishReason="error" /
//     BifrostError chunk that caused the terminal-detect carries no
//     stop_reason info reachable from State without extending the struct
//     (Phase-4 fold-in).
//   - middleware_source=GUARDRAIL — explicit override after Build() (Pitfall 13).
//   - model ← state.Metrics.Model; provider ← state.Metrics.Provider — both
//     threaded from the terminal chat chunk by streaming.State.OnChunk
//     (last-wins-on-non-empty). The real Revenium API rejects model="" with
//     HTTP 400, so a streamed completion now meters with the real model +
//     provider instead of being silently dropped.
//   - identity fields populated via direct struct assignment (Delta A) — same
//     block as BuildSuccess + BuildFailure + BuildBlocked.
func BuildStream(ctx *schemas.BifrostContext, state *streaming.State) *revsdk.MeteringPayload {
	if state == nil {
		return nil
	}

	metrics := state.Finalize()

	// Comma-ok read of the PreLLMHook timestamp with defensive time.Now() fallback.
	start, ok := ctx.Value(ctxkeys.KeyRequestStart).(time.Time)
	if !ok {
		start = time.Now()
	}
	duration := time.Since(start)

	// TTFT: FirstChunkAt - KeyRequestStart in milliseconds. Defensive zero
	// when FirstChunkAt is the zero time (terminal-detect fired before any
	// chat chunk reached OnChunk — should not happen in practice but locks
	// the contract to never emit a negative TTFT).
	var ttftMs int64
	if !metrics.FirstChunkAt.IsZero() {
		ttftMs = metrics.FirstChunkAt.Sub(start).Milliseconds()
	}

	// Model + provider now thread from the terminal chat chunk via
	// streaming.State (last-wins-on-non-empty capture in OnChunk). Emitting a
	// non-blank model is required: the real Revenium API rejects model="".
	model := metrics.Model
	provider := metrics.Provider

	// Build chain — CRITICAL ORDER per Delta 2 (03-RESEARCH.md §1):
	// WithStreaming MUST be called AFTER WithTiming so its non-nil
	// completionStartTime pointer overrides the WithTiming-default that
	// wrote requestTime into CompletionStartTime (SDK payload.go:96-122).
	b := revsdk.NewPayload(revsdk.OperationChat, model, provider)
	b = b.WithTiming(start, duration)
	b = b.WithTokens(int64(metrics.InputTokens), int64(metrics.OutputTokens), int64(metrics.TotalTokens))
	// DELTA 2: WithStreaming MUST be called AFTER WithTiming so its non-nil
	// completionStartTime pointer overrides the WithTiming-default that
	// wrote requestTime into CompletionStartTime (SDK payload.go:96-122).
	b = b.WithStreaming(true, ttftMs, &metrics.FirstChunkAt)
	b = b.WithStopReason("END")
	payload := b.Build()

	// PITFALL-13: SDK default is "revenium-go-sdk" — override to our locked constant.
	payload.MiddlewareSource = MiddlewareSourceGuardrail

	// Identity direct-struct assignment (Delta A) — same block as the three
	// non-streaming builders. Caller identity propagates on streaming
	// completions for analytics.
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
