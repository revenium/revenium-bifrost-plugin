// Package main is the Revenium Bifrost plugin. It exports the six symbols
// Bifrost's plugin loader (framework/plugins/soloader.go) looks up via
// plugin.Lookup: Init, GetName, Cleanup, PreLLMHook, PostLLMHook, and
// HTTPTransportStreamChunkHook. All business logic lives in internal/...;
// this file is a delegator only (Pitfall 20).
//
// Symbol contract (verified against bifrost/core@v1.5.11 +
// framework/plugins/soloader.go on 2026-05-20):
//
//   - Each symbol MUST be a bare `func` declaration at package scope, NOT a
//     method receiver and NOT a `var Hooks = ...`-style struct field. The
//     soloader does a typed plugin.Lookup cast (e.g.,
//     `*plugin.Symbol -> func(any) error`) and any drift in shape causes
//     plugin.Open to fail at load with "failed to cast X to expected
//     signature" — silent in production.
//
//   - Init's `config any` parameter is intentionally ignored. CLAUDE.md
//     "Configuration surface" locks the v1 contract to environment variables
//     only (REVENIUM_METERING_BASE_URL + REVENIUM_METERING_API_KEY); the
//     Bifrost-side config.json plugin entry contributes nothing. T-04-02:
//     discarding the arg is a tampering mitigation for operator-controlled
//     config payloads.
//
//   - Every hook body's FIRST statement is a deferred recoverHook call so a
//     panic in any future Phase 2/3 hook implementation logs at Error with a
//     stack trace and degrades to fail-open (returns the original
//     req/resp/chunk via named return values + a restore callback passed to
//     recoverHook) rather than crashing the Bifrost host worker.
//     CONTRACT-06 + Pitfall 7 + T-04-01.
package main

import (
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	revcore "github.com/revenium/revenium-go-sdk/core"

	"github.com/revenium/revenium-bifrost-plugin/internal/budget"
	"github.com/revenium/revenium-bifrost-plugin/internal/config"
	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
	"github.com/revenium/revenium-bifrost-plugin/internal/identity"
	"github.com/revenium/revenium-bifrost-plugin/internal/metering"
	"github.com/revenium/revenium-bifrost-plugin/internal/plog"
	"github.com/revenium/revenium-bifrost-plugin/internal/streaming"
)

// Package-level state. Set ONCE by Init; read concurrently by hook bodies
// thereafter. No mutex required: the happens-before edge from plugin.Open
// returning successfully to the first hook fire is sufficient (Bifrost loads
// the .so + invokes Init synchronously, then publishes the plugin to the
// hook dispatch pool — the publish is the synchronization point).
var (
	cfg            *config.Config
	budgetClient   *budget.Client
	meteringClient *metering.Client
	logger         plog.Logger

	// panicForTest is a nil-in-production test seam used by the
	// //go:build pluginsanity-tagged TestRecoverHook_FailOpen test
	// (plugin_recover_test.go). Each hook invokes this seam right
	// after the deferred recover-call line so the test can inject a
	// deterministic panic per hook and assert the recover path
	// restores the original input arguments (gap #2 / CONTRACT-06
	// fail-open intent). In production builds the field stays nil and
	// the invocation cost is one nil-check per hook call. The seam
	// MUST NOT be assigned to from any non-test file — enforce via the
	// convention that only plugin_recover_test.go performs the
	// assignment (documented in 01-08-SUMMARY.md threat register).
	panicForTest func(hookName string)
)

// Init is the soloader entry point: `func Init(config any) error`. The
// `config any` argument is the deserialized Bifrost-side plugin config.json
// entry; we intentionally discard it (env-only configuration per
// CLAUDE.md + D-12 + T-04-02). On success the package-level state vars are
// fully wired with REAL clients (per D-12) and Phase 2/3 hook bodies can
// safely reference them.
//
// WR-02 (D-26) atomic-publish / local-build-then-publish: Init constructs all
// dependencies in local variables (localCfg, localLogger, localBudget,
// localMetering) and publishes to the package-level state vars (cfg, logger,
// budgetClient, meteringClient) in a single contiguous block AT THE END of
// the function, only after every constructor succeeded. On any error path,
// already-built clients are Close()'d in rollback and the package vars stay
// zero. Mitigates the resource-leak scenario where Phase 1's incremental
// assignment would have left budgetClient assigned but Cleanup unreachable
// (Init returned an error, so the host abandons the plugin). Bifrost
// serializes plugin.Open invocations, so no sync/atomic.Pointer machinery
// is required — the publish block runs without a concurrent reader.
func Init(_ any) error {
	localCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("revenium-bifrost: config load failed: %w", err)
	}

	localLogger := plog.New(localCfg.Debug)

	// Propagate the plugin's debug flag into the revenium-go-sdk's global
	// logger so its [METERING] debug lines (send target, success/failure,
	// payload summary) surface in the log stream when REVENIUM_DEBUG=true.
	// SetGlobalDebug elevates the SDK level to DEBUG when true and is a
	// no-op when false, so passing the flag unconditionally keeps the call
	// gated by the flag value. Set before the metering client is built so
	// SDK debug logging covers the whole client lifecycle.
	revcore.SetGlobalDebug(localCfg.Debug)

	// Init log MUST NOT include the API key value (T-04-03
	// information-disclosure mitigation; LOG-03 readiness; verified by the
	// plan's grep gate which asserts the literal field reference is absent
	// from this log statement).
	localLogger.Info("revenium-bifrost initializing",
		"base_url", localCfg.BaseURL,
		"dry_run", localCfg.DryRun,
		"debug", localCfg.Debug,
	)

	localBudget := budget.New(localCfg, localLogger)

	localMetering, err := metering.New(localCfg)
	if err != nil {
		// Rollback: drain idle connections on the already-built budget client
		// before returning the error so no resource leaks (T-02-04-07).
		localBudget.Close()
		return fmt.Errorf("revenium-bifrost: metering client init failed: %w", err)
	}

	// All-or-nothing publish: assign every package var in one contiguous
	// block, AFTER every constructor has succeeded. Until this block runs,
	// the package vars stay zero — a half-initialized Init cannot leak
	// state into hook bodies.
	cfg = localCfg
	logger = localLogger
	budgetClient = localBudget
	meteringClient = localMetering

	localLogger.Info("revenium-bifrost initialized successfully")
	return nil
}

// GetName is the soloader entry point: `func GetName() string`. Returns the
// literal "revenium" (CONTRACT-03). This is the string operators reference in
// their Bifrost config.json plugin entry and what appears in Bifrost's
// per-request plugin log stream.
func GetName() string { return "revenium" }

// Cleanup is the soloader entry point: `func Cleanup() error`. Invoked by the
// Bifrost host during graceful shutdown. Calls meteringClient.Close() FIRST so
// the SDK's internal Flush + wg.Wait drains every in-flight metering goroutine
// before the process exits (CONTRACT-05 + Pitfall 11). MUST be synchronous —
// no spawned goroutine, no timeout — because Bifrost awaits this function's
// return before tearing down the process; wrapping in a goroutine would race
// the shutdown signal and lose in-flight metering events. T-04-06 documents
// the trade-off vs. an OPS-01 timeout.
//
// Errors from either Close are logged but not returned: Cleanup's contract is
// best-effort closure of every resource — a partial failure on metering MUST
// NOT skip the budget client's idle-connection drain, and vice versa. The
// returned `error` is always nil today; the signature preserves it for forward
// compatibility if Bifrost ever surfaces Cleanup errors to the operator.
func Cleanup() error {
	if meteringClient != nil {
		if err := meteringClient.Close(); err != nil {
			logger.Error("metering close failed", "err", err)
		}
	}
	if budgetClient != nil {
		budgetClient.Close()
	}
	if logger != nil {
		logger.Info("revenium-bifrost cleaned up")
	}
	return nil
}

// recoverHook is the shared `defer`-target for every hook body. CONTRACT-06 +
// Pitfall 7 + T-04-01: a panic inside any hook MUST NOT crash the Bifrost
// host worker; it logs at Error with the panic value + stack trace and
// degrades to fail-open by invoking the caller-supplied `restore` callback.
//
// The `restore` callback is the mechanism by which the deferring hook's
// named returns are reassigned to the original input arguments. The non-panic
// path runs `restore()` zero times — the only overhead is the deferred call
// prologue/epilogue and one nil-check. On a recovered panic, the Error log
// fires FIRST (so the named returns are still at their pre-restore zero or
// partial values when the log line is built), then `restore()` runs LAST so
// the caller observes the original `(req, resp, chunk)` arguments instead
// of zero values.
//
// Single source of truth: every hook defers a recoverHook call as its FIRST
// statement so future Phase 2/3 hook implementations inherit the safety net
// automatically. The `restore func()` parameter is non-optional — every
// caller MUST supply one or the named-return-restore semantics regress.
func recoverHook(ctx *schemas.BifrostContext, name string, restore func()) {
	if r := recover(); r != nil {
		plog.FromCtx(ctx, logger).Error("hook panic recovered",
			"hook", name,
			"panic", fmt.Sprintf("%v", r),
			"stack", string(debug.Stack()),
		)
		if restore != nil {
			restore()
		}
	}
}

// HTTPTransportPreHook is the soloader entry point fired before every Bifrost
// HTTP-transport-served LLM call, BEFORE PreLLMHook. It is the SOLE surface
// in this plugin that sees raw inbound HTTP headers — *BifrostRequest (the
// type passed to PreLLMHook) carries no headers. Per D-18 we add it as the
// 7th exported soloader symbol; per D-19 it is the writer for the twelve
// `KeyHeader*` ctxkeys declared in internal/ctxkeys, one per identity-bearing
// x-revenium-* header:
//
//   - x-revenium-key-hash                  → KeyHeaderKeyHash       (BUDGET-01)
//   - x-revenium-subscriber-id             → KeyHeaderSubscriberID  (IDENTITY-02)
//   - x-revenium-subscriber-email          → KeyHeaderSubscriberEmail
//   - x-revenium-organization-name         → KeyHeaderOrgName       (IDENTITY-03)
//   - x-revenium-organization-id           → KeyHeaderOrgID
//   - x-revenium-product-name              → KeyHeaderProductName   (IDENTITY-04)
//   - x-revenium-product-id                → KeyHeaderProductID
//   - x-revenium-trace-id                  → KeyHeaderTraceID       (IDENTITY-05)
//   - x-revenium-task-type                 → KeyHeaderTaskType
//   - x-revenium-agent                     → KeyHeaderAgent
//   - x-revenium-subscription-id           → KeyHeaderSubscriptionID
//   - x-revenium-response-quality-score    → KeyHeaderResponseQualityScore
//
// Case-insensitive lookup uses Bifrost's own *HTTPRequest.CaseInsensitiveHeaderLookup
// helper (verified at core@v1.5.11/schemas/plugin.go L51) — Pitfall 9. The
// `*HTTPRequest` value is pooled by Bifrost (sync.Pool); the hook copies the
// header VALUES out as strings and NEVER stores the *HTTPRequest pointer in ctx.
//
// WR-01 (D-25): a nil *BifrostContext is logged at WARN and returns the zero
// values (nil, nil) — fail-open passthrough. Plan 02-04 extends the same
// nil-ctx guard to PreLLMHook / PostLLMHook / HTTPTransportStreamChunkHook.
//
// The hook is a PURE PASSTHROUGH: it ALWAYS returns (nil, nil). Block
// decisions live exclusively in PreLLMHook (Plan 02-04). The recoverHook +
// panicForTest seam mirrors the locked four-line prologue from Phase 1
// hooks. CONTRACT-06 fail-open + T-04-01 panic-mitigation inherited.
func HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (outResp *schemas.HTTPResponse, err error) {
	defer recoverHook(ctx, "HTTPTransportPreHook", func() { outResp, err = nil, nil })
	if panicForTest != nil {
		panicForTest("HTTPTransportPreHook")
	}
	if ctx == nil {
		logger.Warn("HTTPTransportPreHook invoked with nil context — fail-open passthrough")
		return nil, nil
	}

	setIfPresent := func(key schemas.BifrostContextKey, hdr string) {
		if v := req.CaseInsensitiveHeaderLookup(hdr); v != "" {
			ctx.SetValue(key, v)
		}
	}

	// Order matches the D-19 list + internal/ctxkeys/keys.go declaration order.
	setIfPresent(ctxkeys.KeyHeaderKeyHash, "x-revenium-key-hash")
	setIfPresent(ctxkeys.KeyHeaderSubscriberID, "x-revenium-subscriber-id")
	setIfPresent(ctxkeys.KeyHeaderSubscriberEmail, "x-revenium-subscriber-email")
	setIfPresent(ctxkeys.KeyHeaderOrgName, "x-revenium-organization-name")
	setIfPresent(ctxkeys.KeyHeaderOrgID, "x-revenium-organization-id")
	setIfPresent(ctxkeys.KeyHeaderProductName, "x-revenium-product-name")
	setIfPresent(ctxkeys.KeyHeaderProductID, "x-revenium-product-id")
	setIfPresent(ctxkeys.KeyHeaderTraceID, "x-revenium-trace-id")
	setIfPresent(ctxkeys.KeyHeaderTaskType, "x-revenium-task-type")
	setIfPresent(ctxkeys.KeyHeaderAgent, "x-revenium-agent")
	setIfPresent(ctxkeys.KeyHeaderSubscriptionID, "x-revenium-subscription-id")
	setIfPresent(ctxkeys.KeyHeaderResponseQualityScore, "x-revenium-response-quality-score")

	// Single DEBUG log line per request — gated on KeyHeaderKeyHash so
	// non-Revenium-traffic invocations stay quiet. LOG-02 (per-request log)
	// + LOG-03 (truncate key-hash to first 8 chars) honored.
	if keyHash, ok := ctx.Value(ctxkeys.KeyHeaderKeyHash).(string); ok && keyHash != "" {
		plog.FromCtx(ctx, logger).Debug("revenium headers published to ctx",
			"key_hash", plog.TruncateKeyHash(keyHash))
	}

	return nil, nil
}

// PreLLMHook is the soloader entry point fired before every LLM call. It is
// the Phase 2 4-branch budget-enforcement orchestrator that ports the Python
// ReveniumGuardrail behavior (revenium-middleware-litellm-proxy-python's
// guardrail.py L137-195) into Bifrost's plugin hook model:
//
//   - Branch 1 (BUDGET-01 + IDENTITY-01): missing x-revenium-key-hash →
//     log at Debug + return passthrough WITHOUT issuing the budget HTTP call.
//     identity.ExtractKeyHash is the single chokepoint for "header absent"
//     semantics — when HTTPTransportPreHook did not fire (non-HTTP entry
//     path) ALL KeyHeader* ctxkeys are absent and ExtractKeyHash returns "".
//   - Branch 2 (BUDGET-04 + BUDGET-05): *budget.ErrTransport / non-200 /
//     timeout / decode failure → WARN log + fail-open passthrough. The
//     dispatch is errors.As(checkErr, &transportErr) per Pitfall 8 typed-
//     sentinel discipline. Mirrors CLAUDE.md "fail-open on budget-check
//     transport errors, non-200 responses, and timeouts (production
//     availability over enforcement)".
//   - Branch 3 (BUDGET-03 + BUDGET-06 + CONFIG-03 dry-run): blocked
//     decision. Writes ctxkeys.KeyShortCircuited (or KeyDryRunDecision when
//     cfg.DryRun) carrying the *budget.Decision pointer — this is the
//     LOCKED Phase 3 reader contract. Phase 3's PostLLMHook will
//     type-assert ctx.Value(ctxkeys.KeyShortCircuited).(*budget.Decision)
//     and dispatch the metering blocked-path builder to avoid double-counting
//     blocks as billable LLM calls (METER-03). The short-circuit value comes
//     from budget.BuildShortCircuit and carries StatusCode=&429 +
//     AllowFallbacks=&false (verified at the wire by Phase 1 spike
//     TestContract_429NoFallback).
//   - Branch 4 (BUDGET-02 happy path): Decision{Allowed:true} → log at
//     Debug + passthrough.
//
// WR-01 (D-25): nil ctx logs at WARN and returns the original request
// unchanged. Placed AFTER defer recoverHook + panicForTest seam, BEFORE
// any line that dereferences ctx.
//
// KeyRequestStart preservation: ctx.SetValue(KeyRequestStart, time.Now())
// remains in this hook — Phase 3's PostLLMHook (non-streaming RTT) and
// HTTPTransportStreamChunkHook (TTFT) both consume it. Moved AFTER the
// WR-01 nil-guard so nil-ctx invocation cannot deref.
//
// Pitfall 7 (forcetypeassert): every ctx.Value cast in the consumed
// identity.ExtractKeyHash helper uses comma-ok form. Pitfall 8 (swallowed
// block): the only path that returns a non-nil short-circuit is Branch 3
// — recoverHook MUST NOT swallow it, which is structurally true because
// the recover branch only fires on panic, never on a normal return.
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (outReq *schemas.BifrostRequest, sc *schemas.LLMPluginShortCircuit, err error) {
	defer recoverHook(ctx, "PreLLMHook", func() { outReq, sc, err = req, nil, nil })
	if panicForTest != nil {
		panicForTest("PreLLMHook")
	}
	if ctx == nil {
		logger.Warn("PreLLMHook invoked with nil context — fail-open passthrough")
		return req, nil, nil
	}

	log := plog.FromCtx(ctx, logger)
	ctx.SetValue(ctxkeys.KeyRequestStart, time.Now())
	// Phase 3 (Delta D): pre-stash RequestType so PostLLMHook can detect streaming without access to *BifrostRequest.
	ctx.SetValue(ctxkeys.KeyRequestType, req.RequestType)
	// quick-260617-vhq: stash the requested model/provider BEFORE the
	// budget-check branches so the BUDGET_EXCEEDED block event (metering.
	// BuildBlocked) can emit a non-blank model — the real Revenium API rejects
	// model="" with HTTP 400. req.ChatRequest is a pointer per the
	// BifrostRequest oneof shape; nil-guard it (non-chat request types carry a
	// nil ChatRequest).
	if req.ChatRequest != nil {
		ctx.SetValue(ctxkeys.KeyRequestModel, req.ChatRequest.Model)
		ctx.SetValue(ctxkeys.KeyRequestProvider, string(req.ChatRequest.Provider))
	}
	// Phase 3 streaming lifecycle: allocate per-stream State accumulator only when this is a streaming request; StreamChunkHook reads via comma-ok-typed assert.
	if req.RequestType == schemas.ChatCompletionStreamRequest {
		ctx.SetValue(ctxkeys.KeyStreamState, streaming.New())
	}

	// Branch 1 (BUDGET-01 + IDENTITY-01): missing key-hash → skip budget check + allow.
	keyHash := identity.ExtractKeyHash(ctx)
	if keyHash == "" {
		log.Debug("no key-hash header — skipping budget check")
		return req, nil, nil
	}
	keyHashTrunc := plog.TruncateKeyHash(keyHash)

	decision, checkErr := budgetClient.Check(ctx, keyHash)

	// Branch 2 (BUDGET-04 + BUDGET-05): *budget.ErrTransport → WARN + fail-open.
	var transportErr *budget.ErrTransport
	if errors.As(checkErr, &transportErr) {
		log.Warn("budget check failed — failing open",
			"key_hash", keyHashTrunc, "err", transportErr.Error())
		return req, nil, nil
	}
	// Defensive: any non-nil error that is NOT *ErrTransport is a programmer
	// error per Plan 02-03's typed-sentinel discipline. Fail open with a WARN
	// rather than propagating the unknown error type.
	if checkErr != nil {
		log.Warn("budget check returned unexpected error type — failing open",
			"key_hash", keyHashTrunc, "err", checkErr.Error())
		return req, nil, nil
	}

	// Branch 3 (BUDGET-03 + Phase 3 contract): blocked. The dry-run sub-branch
	// (CONFIG-03) writes to KeyDryRunDecision instead of KeyShortCircuited so
	// Phase 3's PostLLMHook can attach a "would_have_blocked" flag to the
	// metering payload without short-circuiting the request.
	if decision != nil && !decision.Allowed {
		if cfg.DryRun {
			log.Warn("budget exceeded but DRY_RUN=true — allowing",
				"key_hash", keyHashTrunc, "exceeded_count", decision.ExceededCount)
			ctx.SetValue(ctxkeys.KeyDryRunDecision, decision)
			return req, nil, nil
		}
		log.Warn("budget exceeded — blocking",
			"key_hash", keyHashTrunc, "exceeded_count", decision.ExceededCount)
		// LOCKED Phase 3 contract: KeyShortCircuited payload type is *budget.Decision.
		ctx.SetValue(ctxkeys.KeyShortCircuited, decision)
		return req, budget.BuildShortCircuit(decision), nil
	}

	// Branch 4 (BUDGET-02 happy path): allowed.
	log.Debug("budget within limit — allowing", "key_hash", keyHashTrunc)
	return req, nil, nil
}

// PostLLMHook is the soloader entry point fired after every LLM call (even
// after a PreLLMHook short-circuit per Bifrost's symmetry guarantee — Phase 1
// spike D-15.1). It is the Phase 3 5-branch non-streaming metering dispatcher
// that ports the Python ReveniumGuardrail post-call behavior (guardrail.py
// L197-489) into Bifrost's plugin hook model.
//
// 5-branch dispatch (ORDER LOCKED — see source rationale comment inline):
//
//   - Branch 1 (WR-01 nil-ctx): pre-existing fail-open passthrough; preserved
//     verbatim from the Phase 1/2 prologue.
//   - Branch 2 (METER-05 streaming no-op): when KeyRequestType ==
//     ChatCompletionStreamRequest, return passthrough with ZERO emissions.
//     HTTPTransportStreamChunkHook (Plan 03-03) is the sole streaming
//     roll-up emitter because Bifrost dispatches chunk hooks AFTER
//     PostLLMHook on streamed responses (Phase 1 spike D-15.2).
//   - Branch 3 (METER-03 KeyShortCircuited + Pitfall 1 anti-double-count):
//     when PreLLMHook short-circuited with a *budget.Decision, emit ONE
//     BUDGET_EXCEEDED event via the blocked-path builder and return.
//     Comma-ok type assertion (Pitfall 7) defends against a co-loaded
//     plugin overwriting KeyShortCircuited with a non-*budget.Decision
//     payload (T-03-02-01 mitigation). The early return is the critical
//     mechanism that prevents a blocked request from being double-counted
//     as ONE block + ONE zero-token success.
//   - Branch 4 (METER-02): when bifrostErr != nil, emit ONE ERROR event
//     via the failure-path builder.
//   - Branch 5 (METER-01): when resp != nil, emit ONE END event via the
//     success-path builder.
//
// Logging discipline (Pitfall 12): every per-branch outcome log is Debug
// (per-request granularity, never INFO). meteringClient.Send is
// fire-and-forget (no error return — verified Plan 01-03 SDK decision); the
// SDK's internal `core.Error` logger surfaces transport failures. We do NOT
// log Send failures from here (no return value to check).
func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (outResp *schemas.BifrostResponse, outErr *schemas.BifrostError, err error) {
	defer recoverHook(ctx, "PostLLMHook", func() { outResp, outErr, err = resp, bifrostErr, nil })
	if panicForTest != nil {
		panicForTest("PostLLMHook")
	}
	if ctx == nil {
		logger.Warn("PostLLMHook invoked with nil context — fail-open passthrough")
		return resp, bifrostErr, nil
	}

	log := plog.FromCtx(ctx, logger)

	// Branch ordering rationale: streaming-no-op MUST come before
	// KeyShortCircuited because a budget block aborts BEFORE streaming begins
	// (verified Phase 1 spike D-15.2 + 03-RESEARCH.md §3 execution-order
	// proof). Listed first so the no-op is cheap on the streaming hot path.
	// If both flags are ever observed on the same ctx (shouldn't happen — a
	// budget short-circuit returns 429 BEFORE any chunk dispatch), the
	// streaming-no-op-first ordering means StreamChunkHook will not fire on
	// a short-circuited request and no metering loss results.

	// Branch 2 (METER-05 streaming no-op). Read RequestType via the ctxkey
	// pre-stashed by PreLLMHook (03-RESEARCH.md Delta D — PostLLMHook does
	// not receive *BifrostRequest). Comma-ok defends against a co-loaded
	// plugin overwriting KeyRequestType with a non-RequestType type
	// (T-03-02-02 mitigation: degrades to "not detected as streaming → fall
	// through to success branch" rather than panicking).
	if rt, ok := ctx.Value(ctxkeys.KeyRequestType).(schemas.RequestType); ok && rt == schemas.ChatCompletionStreamRequest {
		log.Debug("PostLLMHook no-op for streaming — StreamChunkHook owns roll-up")
		return resp, bifrostErr, nil
	}

	// Branch 3 (METER-03 KeyShortCircuited — Pitfall 1 anti-double-count).
	// Phase 2 D-26 locked ctxkeys.KeyShortCircuited payload type as
	// *budget.Decision pointer. Comma-ok defends against a co-loaded plugin
	// overwriting with a non-*budget.Decision type (T-03-02-01).
	if v := ctx.Value(ctxkeys.KeyShortCircuited); v != nil {
		if d, ok := v.(*budget.Decision); ok && d != nil {
			if p := metering.BuildBlocked(ctx, d); p != nil {
				meteringClient.Send(p)
				log.Debug("metered BUDGET_EXCEEDED")
			}
			return resp, bifrostErr, nil
		}
	}

	// Branch 4 (METER-02 failure path).
	if bifrostErr != nil {
		if p := metering.BuildFailure(ctx, bifrostErr); p != nil {
			meteringClient.Send(p)
			log.Debug("metered ERROR")
		}
		return resp, bifrostErr, nil
	}

	// Branch 5 (METER-01 success path). BuildSuccess returns nil when
	// resp.ChatResponse is nil (Delta B — non-chat response variant); caller
	// MUST skip Send on a nil return.
	if resp != nil {
		if p := metering.BuildSuccess(ctx, resp); p != nil {
			meteringClient.Send(p)
			log.Debug("metered END")
		}
	}
	return resp, bifrostErr, nil
}

// HTTPTransportStreamChunkHook is the soloader entry point fired for every
// streaming chunk Bifrost forwards back to the caller. Phase 3 implementation:
// per-chunk state accumulation + single terminal-chunk roll-up emission per
// METER-04 (Delta C unified detector — Bifrost normalizes provider-specific
// terminal markers BEFORE the chunk reaches this plugin).
//
// Algorithm:
//
//  1. Read the per-stream State accumulator written by PreLLMHook on the
//     streaming branch. Comma-ok defends against a co-loaded plugin
//     overwriting KeyStreamState with a non-pointer payload (Pitfall 7 +
//     T-03-03-01 mitigation — degrades to pass-through on type mismatch).
//  2. Drive the per-chunk accumulator on every chunk — the accumulator's
//     own nil/non-chat defenses handle malformed chunks per T-03-03-02.
//  3. On the unified-detector terminal-chunk branch emit ONE streaming
//     roll-up payload via meteringClient.Send (fire-and-forget). Pitfall 2
//     single-emitter contract: PostLLMHook (Plan 03-02) is the no-op for
//     streaming; this hook is the sole roll-up emitter because Bifrost
//     dispatches chunk hooks AFTER PostLLMHook on streamed responses
//     (Phase 1 spike D-15.2).
//
// Per-chunk hot path: this hook fires once per chunk for the full lifetime
// of the streamed completion. Keep allocations minimal — state.OnChunk
// accumulates in place without allocating new strings/maps per chunk. The
// terminal-chunk branch allocates ONCE per stream (the *MeteringPayload).
//
// Concurrency contract: Bifrost dispatches StreamChunkHook serially per
// request (Research §3 execution order + spike D-15.3 ctx round-trip), so
// per-request State is single-threaded and unguarded (T-03-03-03 disposition:
// accept, with documentation in state.go).
func HTTPTransportStreamChunkHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (outChunk *schemas.BifrostStreamChunk, err error) {
	defer recoverHook(ctx, "HTTPTransportStreamChunkHook", func() { outChunk, err = chunk, nil })
	if panicForTest != nil {
		panicForTest("HTTPTransportStreamChunkHook")
	}
	if ctx == nil {
		logger.Warn("HTTPTransportStreamChunkHook invoked with nil context — fail-open passthrough")
		return chunk, nil
	}

	log := plog.FromCtx(ctx, logger)

	// Comma-ok read of the State accumulator written by PreLLMHook on the
	// streaming branch. Defensive: if the key is absent OR a co-loaded plugin
	// overwrote it with a non-*streaming.State value, pass through silently
	// (T-03-03-01 mitigation).
	state, ok := ctx.Value(ctxkeys.KeyStreamState).(*streaming.State)
	if !ok || state == nil {
		return chunk, nil
	}

	// Per-chunk accumulation — OnChunk handles nil chunks and non-chat variants defensively.
	state.OnChunk(chunk)

	// METER-04 terminal-chunk branch (Delta C unified detector — BifrostError OR FinishReason set on any choice). Single emission per stream.
	if streaming.IsTerminal(chunk) {
		if p := metering.BuildStream(ctx, state); p != nil {
			meteringClient.Send(p)
			log.Debug("metered streaming roll-up", "chunks", state.Metrics.TotalChunks)
		}
	}

	_ = req // req carries no per-chunk identity data; StreamChunkHook reads identity from ctx (set by HTTPTransportPreHook).
	return chunk, nil
}
