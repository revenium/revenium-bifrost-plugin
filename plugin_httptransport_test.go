//go:build pluginsanity

package main

// TestHTTPTransportPreHook_PublishesAllHeaders + the supporting subtests
// lock the Plan 02-02 contract:
//
//   1. HTTPTransportPreHook is a top-level func declaration in package main
//      with the exact soloader-expected signature
//      `func(ctx *schemas.BifrostContext, req *schemas.HTTPRequest)
//          (*schemas.HTTPResponse, error)`.
//   2. The hook is a pure passthrough — every invocation returns (nil, nil).
//   3. All twelve x-revenium-* headers from D-19 are case-insensitively
//      looked up via Bifrost's *HTTPRequest.CaseInsensitiveHeaderLookup and,
//      when present + non-empty, copied into the BifrostContext via the
//      twelve corresponding ctxkeys.KeyHeader* constants (Plan 02-01).
//   4. Headers that are absent from the request leave the matching ctxkey
//      untouched (no zero-value writes that would mask future SetValue
//      reorderings).
//   5. The hook starts with the locked four-line prologue per D-25/D-28:
//        - defer recoverHook(ctx, "HTTPTransportPreHook", ...)
//        - panicForTest seam invocation
//        - WR-01 nil-ctx guard (logger.Warn + early return (nil, nil))
//        - body
//      The first three subtests below exercise the body; the nil-ctx
//      subtest exercises the WR-01 guard.
//
// Invocation: `go test -tags pluginsanity -run TestHTTPTransportPreHook
// -count=1 .` — the //go:build pluginsanity tag keeps this test file out
// of the default `go test ./...` path (matches Plan 01-08 precedent).

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
)

// allTwelveHeaders is the exhaustive D-19 mapping the hook must publish.
// Order matches Plan 02-01's keys.go declaration order (KeyHeaderKeyHash
// first, KeyHeaderResponseQualityScore last) — same as the setIfPresent
// invocation order required by the plan.
var allTwelveHeaders = []struct {
	ctxKey schemas.BifrostContextKey
	hdr    string
	val    string // distinct per row so any cross-wiring fails LOUDLY
}{
	{ctxkeys.KeyHeaderKeyHash, "x-revenium-key-hash", "kh-AAAAAAAAAAA-zzz"},
	{ctxkeys.KeyHeaderSubscriberID, "x-revenium-subscriber-id", "sub-id-001"},
	{ctxkeys.KeyHeaderSubscriberEmail, "x-revenium-subscriber-email", "user@example.com"},
	{ctxkeys.KeyHeaderOrgName, "x-revenium-organization-name", "Acme Inc."},
	{ctxkeys.KeyHeaderOrgID, "x-revenium-organization-id", "org-002"},
	{ctxkeys.KeyHeaderProductName, "x-revenium-product-name", "Widget Pro"},
	{ctxkeys.KeyHeaderProductID, "x-revenium-product-id", "prod-003"},
	{ctxkeys.KeyHeaderTraceID, "x-revenium-trace-id", "trace-004"},
	{ctxkeys.KeyHeaderTaskType, "x-revenium-task-type", "chat.completion"},
	{ctxkeys.KeyHeaderAgent, "x-revenium-agent", "agent-v1"},
	{ctxkeys.KeyHeaderSubscriptionID, "x-revenium-subscription-id", "subs-005"},
	{ctxkeys.KeyHeaderResponseQualityScore, "x-revenium-response-quality-score", "0.92"},
}

func TestHTTPTransportPreHook_PublishesAllHeaders(t *testing.T) {
	// Save and defer-restore package-level state (matches plan 01-08 pattern).
	origLogger := logger
	origSeam := panicForTest
	t.Cleanup(func() {
		logger = origLogger
		panicForTest = origSeam
	})

	t.Run("all_twelve_present_lowercase_canonical", func(t *testing.T) {
		// Build the request with every header at its canonical lowercase
		// form (the form CaseInsensitiveHeaderLookup hits in the first
		// exact-match branch — fastest path).
		headers := make(map[string]string, len(allTwelveHeaders))
		for _, h := range allTwelveHeaders {
			headers[h.hdr] = h.val
		}
		req := &schemas.HTTPRequest{Headers: headers}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

		gotResp, gotErr := HTTPTransportPreHook(ctx, req)

		// Pure passthrough contract — both returns nil, always.
		require.Nil(t, gotResp, "HTTPTransportPreHook must return nil *HTTPResponse (pure passthrough)")
		require.Nil(t, gotErr, "HTTPTransportPreHook must return nil error (pure passthrough)")

		// Every one of the twelve ctxkeys MUST carry the exact value from
		// the matching header. Comma-ok cast + value compare so a
		// wrong-type write would fail LOUDLY here (Pitfall 7).
		for _, h := range allTwelveHeaders {
			got, ok := ctx.Value(h.ctxKey).(string)
			require.True(t, ok, "ctxkey %q must carry a string (Pitfall 7)", h.ctxKey)
			assert.Equal(t, h.val, got, "header %q must round-trip via ctxkey %q", h.hdr, h.ctxKey)
		}
	})

	t.Run("mixed_case_headers_resolve_via_case_insensitive_lookup", func(t *testing.T) {
		// All twelve headers supplied at MIXED case — exercises
		// CaseInsensitiveHeaderLookup's lowered-key + EqualFold branches.
		// IDENTITY-01 + Pitfall 9 contract.
		headers := make(map[string]string, len(allTwelveHeaders))
		mixed := []string{
			"X-Revenium-Key-Hash",
			"X-REVENIUM-Subscriber-ID",
			"X-revenium-Subscriber-Email",
			"x-Revenium-Organization-Name",
			"X-REVENIUM-ORGANIZATION-ID",
			"X-Revenium-Product-Name",
			"x-revenium-Product-Id",
			"X-Revenium-Trace-Id",
			"X-Revenium-Task-Type",
			"x-revenium-AGENT",
			"X-Revenium-Subscription-Id",
			"X-Revenium-Response-Quality-Score",
		}
		require.Len(t, mixed, len(allTwelveHeaders), "test self-check: mixed-case list must align with allTwelveHeaders")
		for i, h := range allTwelveHeaders {
			headers[mixed[i]] = h.val
		}
		req := &schemas.HTTPRequest{Headers: headers}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

		gotResp, gotErr := HTTPTransportPreHook(ctx, req)
		require.Nil(t, gotResp)
		require.Nil(t, gotErr)

		for _, h := range allTwelveHeaders {
			got, ok := ctx.Value(h.ctxKey).(string)
			require.True(t, ok, "mixed-case header for ctxkey %q must still resolve via CaseInsensitiveHeaderLookup", h.ctxKey)
			assert.Equal(t, h.val, got, "mixed-case header for ctxkey %q must round-trip", h.ctxKey)
		}
	})

	t.Run("absent_headers_do_not_populate_ctxkeys", func(t *testing.T) {
		// Only KeyHash supplied; the other eleven ctxkeys MUST remain
		// absent (typed-nil from ctx.Value) — proves setIfPresent
		// guards against empty-string writes that would mask Phase 3
		// nil-vs-empty distinctions.
		req := &schemas.HTTPRequest{Headers: map[string]string{
			"x-revenium-key-hash": "kh-only-this-one",
		}}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

		gotResp, gotErr := HTTPTransportPreHook(ctx, req)
		require.Nil(t, gotResp)
		require.Nil(t, gotErr)

		// KeyHeaderKeyHash present.
		v, ok := ctx.Value(ctxkeys.KeyHeaderKeyHash).(string)
		require.True(t, ok)
		assert.Equal(t, "kh-only-this-one", v)

		// All eleven other KeyHeader* ctxkeys absent.
		for _, h := range allTwelveHeaders[1:] {
			got := ctx.Value(h.ctxKey)
			assert.Nil(t, got, "ctxkey %q must remain absent when its header is missing (no empty-string write)", h.ctxKey)
		}
	})

	t.Run("empty_string_header_value_does_not_populate_ctxkey", func(t *testing.T) {
		// Header present but value is empty string — setIfPresent's
		// `if v != ""` branch must skip the write.
		req := &schemas.HTTPRequest{Headers: map[string]string{
			"x-revenium-key-hash":     "kh-real",
			"x-revenium-subscriber-id": "", // explicit empty
		}}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

		gotResp, gotErr := HTTPTransportPreHook(ctx, req)
		require.Nil(t, gotResp)
		require.Nil(t, gotErr)

		v, _ := ctx.Value(ctxkeys.KeyHeaderKeyHash).(string)
		assert.Equal(t, "kh-real", v)

		got := ctx.Value(ctxkeys.KeyHeaderSubscriberID)
		assert.Nil(t, got, "empty-string header value must NOT write the ctxkey (setIfPresent's empty guard)")
	})

	t.Run("no_headers_at_all_still_returns_nil_nil_no_panic", func(t *testing.T) {
		// Empty headers map — happy-path quiet behavior. Hook must not
		// panic (recoverHook is the safety net of last resort) and
		// must not emit the DEBUG log line (KeyHash absent → branch skipped).
		req := &schemas.HTTPRequest{Headers: map[string]string{}}
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

		require.NotPanics(t, func() {
			gotResp, gotErr := HTTPTransportPreHook(ctx, req)
			require.Nil(t, gotResp)
			require.Nil(t, gotErr)
		})

		// Every KeyHeader* ctxkey untouched.
		for _, h := range allTwelveHeaders {
			assert.Nil(t, ctx.Value(h.ctxKey), "ctxkey %q must remain absent when no headers were supplied", h.ctxKey)
		}
	})
}

func TestHTTPTransportPreHook_NilContextGuard_FailOpenPassthrough(t *testing.T) {
	// WR-01 (D-25): nil-ctx invocation MUST log at WARN and return
	// (nil, nil) without dereferencing ctx. Routing through the
	// capturing fallback Logger (per `plog.FromCtx(nil, fallback) ==
	// fallback` per Plan 01-02 plog) lets us assert the log shape.
	origLogger := logger
	origSeam := panicForTest
	t.Cleanup(func() {
		logger = origLogger
		panicForTest = origSeam
	})

	cap := &capturingLogger{}
	logger = cap
	panicForTest = nil

	req := &schemas.HTTPRequest{Headers: map[string]string{
		"x-revenium-key-hash": "kh-irrelevant-because-ctx-is-nil",
	}}

	var ctx *schemas.BifrostContext // explicitly nil
	gotResp, gotErr := HTTPTransportPreHook(ctx, req)
	require.Nil(t, gotResp, "WR-01 nil-ctx guard must return nil *HTTPResponse")
	require.Nil(t, gotErr, "WR-01 nil-ctx guard must return nil error")

	// Exactly one WARN log line with the locked literal message.
	entries := cap.entries()
	warnEntries := filterByLevel(entries, "WARN")
	require.Len(t, warnEntries, 1, "WR-01 nil-ctx guard must emit exactly one WARN log line")
	assert.Equal(t, "HTTPTransportPreHook invoked with nil context — fail-open passthrough", warnEntries[0].msg,
		"WR-01 nil-ctx guard log message must match the locked literal (D-25)")
}

func TestHTTPTransportPreHook_PanicRecover_FailOpen(t *testing.T) {
	// Extends the Plan 01-08 TestRecoverHook_FailOpen contract to the
	// new HTTPTransportPreHook hook: a panic injected via panicForTest
	// MUST recover, log at Error with hook=HTTPTransportPreHook +
	// panic value + non-empty stack trace, and the hook's named
	// returns MUST be (nil, nil) per the restore callback contract.
	origLogger := logger
	origSeam := panicForTest
	t.Cleanup(func() {
		logger = origLogger
		panicForTest = origSeam
	})

	cap := &capturingLogger{}
	logger = cap
	panicForTest = func(name string) {
		if name == "HTTPTransportPreHook" {
			panic("test-httptransport-panic")
		}
	}

	// nil ctx routes the Error log through the capturing fallback
	// (per plog.FromCtx(nil, fallback) == fallback) without
	// touching ctx.Log. panicForTest fires BEFORE any line that
	// dereferences ctx, so nil-ctx is safe for the panic subtest
	// (same trick used by Plan 01-08's panic subtests).
	var ctx *schemas.BifrostContext
	req := &schemas.HTTPRequest{Headers: map[string]string{}}
	gotResp, gotErr := HTTPTransportPreHook(ctx, req)

	// Fail-open contract: the restore callback re-zeros the named
	// returns to (nil, nil) for HTTPTransportPreHook (since the
	// pure-passthrough success path also returns (nil, nil), the
	// restore is structurally identical — the test value comes from
	// proving the recover branch ran and DID NOT propagate the panic).
	require.Nil(t, gotResp, "HTTPTransportPreHook must return nil *HTTPResponse after recovered panic")
	require.Nil(t, gotErr, "HTTPTransportPreHook hook-error return must be nil on recover")

	entries := cap.entries()
	errEntries := filterByLevel(entries, "ERROR")
	require.Len(t, errEntries, 1, "exactly one Error log expected on recovered panic")
	assertRecoverLogShape(t, errEntries[0], "HTTPTransportPreHook", "test-httptransport-panic")
}
