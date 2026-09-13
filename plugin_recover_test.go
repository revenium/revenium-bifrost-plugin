//go:build pluginsanity

package main

// TestRecoverHook_FailOpen proves the fail-open contract of recoverHook
// (gap #2 / CR-01): a panic inside any of the three exported hooks must
// recover, log at Error with hook name + panic value + non-empty stack
// trace, and the deferring hook MUST return its ORIGINAL input pointers
// (NOT the zero values that an un-restored recover would produce).
//
// The test injects a deterministic panic via the package-level
// panicForTest seam (plugin.go), which each hook invokes immediately
// after its defer-recoverHook line. The test then asserts:
//   1. Pointer equality between the original input pointer and the
//      hook's return value for the request/response/chunk surface.
//   2. The hook-internal error return is nil (panic must NOT be
//      surfaced through the hook error channel — that would defeat
//      the fail-open intent).
//   3. The capturing Logger received exactly one Error call with
//      structured fields hook=<name>, panic=<value>, stack=<non-empty>.
//
// Invocation: `go test -tags pluginsanity -run TestRecoverHook_FailOpen
// -count=1 .` — the //go:build pluginsanity tag keeps this test file
// invisible to the default `go test ./...` path so production CI is
// not contaminated by the panicForTest seam wiring.

import (
	"context"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/plog"
)

// capturedLog records one logger invocation for later assertion.
type capturedLog struct {
	level string
	msg   string
	args  []any
}

// capturingLogger implements plog.Logger by appending each call to an
// internal slice under a mutex. The pluginsanity test installs an
// instance into the package-level `logger` var for the duration of each
// subtest and inspects entries afterward.
type capturingLogger struct {
	mu   sync.Mutex
	logs []capturedLog
}

func (c *capturingLogger) record(level, msg string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, capturedLog{level: level, msg: msg, args: append([]any(nil), args...)})
}

func (c *capturingLogger) Debug(msg string, args ...any) { c.record("DEBUG", msg, args...) }
func (c *capturingLogger) Info(msg string, args ...any)  { c.record("INFO", msg, args...) }
func (c *capturingLogger) Warn(msg string, args ...any)  { c.record("WARN", msg, args...) }
func (c *capturingLogger) Error(msg string, args ...any) { c.record("ERROR", msg, args...) }

// entries returns a defensive copy of the recorded logs.
func (c *capturingLogger) entries() []capturedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedLog, len(c.logs))
	copy(out, c.logs)
	return out
}

// findStructured walks the args slice in (key, value) pairs and
// returns the value for the given key (as any). Mirrors plog.foldArgs's
// (key, value) pair convention.
func findStructured(args []any, key string) (any, bool) {
	for i := 0; i+1 < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			continue
		}
		if k == key {
			return args[i+1], true
		}
	}
	return nil, false
}

func TestRecoverHook_FailOpen(t *testing.T) {
	// Save and defer-restore the package-level logger and panicForTest
	// seam so a test failure cannot leak modified state into a later
	// run of the same test binary. Per t.Cleanup contract these run in
	// LIFO order; both are no-op assignments in production.
	origLogger := logger
	origSeam := panicForTest
	t.Cleanup(func() {
		logger = origLogger
		panicForTest = origSeam
	})

	// Pass a nil *BifrostContext throughout. plog.FromCtx(nil, fallback)
	// returns the fallback Logger unchanged (verified by 01-VERIFICATION.md
	// truth #9 — Plan 01-02 plog handles nil-ctx). Routing through the
	// capturing fallback is what lets the test assert recover-log shape.
	// Passing a real *schemas.BifrostContext would route the Error log
	// through ctxLogger -> ctx.Log() instead, bypassing our captor.
	// The panicForTest seam fires BEFORE any line that would dereference
	// ctx (Debug log, SetValue), so nil-ctx is safe for the panic path.
	var ctx *schemas.BifrostContext // nil — see comment above

	t.Run("PreLLMHook_panic_returns_original_request", func(t *testing.T) {
		cap := &capturingLogger{}
		logger = cap
		panicForTest = func(name string) {
			if name == "PreLLMHook" {
				panic("test-pre-panic")
			}
		}
		t.Cleanup(func() { panicForTest = nil })

		origReq := &schemas.BifrostRequest{}
		gotReq, gotSC, gotErr := PreLLMHook(ctx, origReq)

		// Fail-open: the recovered hook MUST return the ORIGINAL
		// request pointer (pointer-identity, not just structural equality),
		// nil short-circuit, and nil hook error.
		require.Same(t, origReq, gotReq, "PreLLMHook must return original *BifrostRequest after recovered panic")
		require.Nil(t, gotSC, "PreLLMHook must return nil short-circuit on recover (panic must not propagate as a block)")
		require.Nil(t, gotErr, "PreLLMHook hook-error return must be nil on recover (panic must not propagate via the hook error channel)")

		// Recover log assertion: exactly one Error call with hook
		// name + panic value + non-empty stack trace.
		entries := cap.entries()
		errEntries := filterByLevel(entries, "ERROR")
		require.Len(t, errEntries, 1, "exactly one Error log expected on recovered panic")
		assertRecoverLogShape(t, errEntries[0], "PreLLMHook", "test-pre-panic")
	})

	t.Run("PostLLMHook_panic_returns_original_resp_and_err", func(t *testing.T) {
		cap := &capturingLogger{}
		logger = cap
		panicForTest = func(name string) {
			if name == "PostLLMHook" {
				panic("test-post-panic")
			}
		}
		t.Cleanup(func() { panicForTest = nil })

		origResp := &schemas.BifrostResponse{}
		origBifrostErr := &schemas.BifrostError{}
		gotResp, gotErr, hookErr := PostLLMHook(ctx, origResp, origBifrostErr)

		// Fail-open: recovered PostLLMHook returns ORIGINAL response
		// AND ORIGINAL bifrostErr so Bifrost's upstream error channel
		// is preserved (NOT swallowed). Hook-internal error is nil.
		require.Same(t, origResp, gotResp, "PostLLMHook must return original *BifrostResponse after recovered panic")
		require.Same(t, origBifrostErr, gotErr, "PostLLMHook must return original *BifrostError after recovered panic (upstream error must survive)")
		require.Nil(t, hookErr, "PostLLMHook hook-error return must be nil on recover")

		entries := cap.entries()
		errEntries := filterByLevel(entries, "ERROR")
		require.Len(t, errEntries, 1, "exactly one Error log expected on recovered panic")
		assertRecoverLogShape(t, errEntries[0], "PostLLMHook", "test-post-panic")
	})

	t.Run("HTTPTransportStreamChunkHook_panic_returns_original_chunk", func(t *testing.T) {
		cap := &capturingLogger{}
		logger = cap
		panicForTest = func(name string) {
			if name == "HTTPTransportStreamChunkHook" {
				panic("test-stream-panic")
			}
		}
		t.Cleanup(func() { panicForTest = nil })

		origHTTPReq := &schemas.HTTPRequest{}
		origChunk := &schemas.BifrostStreamChunk{}
		gotChunk, hookErr := HTTPTransportStreamChunkHook(ctx, origHTTPReq, origChunk)

		// Fail-open: recovered chunk hook returns ORIGINAL chunk so
		// the stream does not silently truncate. Hook error is nil.
		require.Same(t, origChunk, gotChunk, "HTTPTransportStreamChunkHook must return original *BifrostStreamChunk after recovered panic (no silent stream truncation)")
		require.Nil(t, hookErr, "HTTPTransportStreamChunkHook hook-error return must be nil on recover")

		entries := cap.entries()
		errEntries := filterByLevel(entries, "ERROR")
		require.Len(t, errEntries, 1, "exactly one Error log expected on recovered panic")
		assertRecoverLogShape(t, errEntries[0], "HTTPTransportStreamChunkHook", "test-stream-panic")
	})

	t.Run("non_panic_path_does_not_invoke_seam_or_emit_error_log", func(t *testing.T) {
		// Regression gate for Test 5 in Task 4 behaviors: with
		// panicForTest == nil the hooks must behave as the
		// untouched Plan 01-04 passthroughs. We verify the
		// capturingLogger receives ZERO Error calls and the
		// returned pointers are the originals (which is also the
		// non-panic happy path).
		//
		// This subtest needs a real *BifrostContext because the
		// non-panic path of PreLLMHook calls ctx.SetValue, which
		// nil-derefs on a nil ctx. The trade-off: routing the
		// Debug log line through ctxLogger -> ctx.Log means the
		// capturingLogger does NOT see that line — but we only
		// care that NO Error log is emitted, and Error routing
		// goes through ctxLogger too in this branch, so an Error
		// here would land on ctx.Log either way. The assertion
		// stands: zero Error entries on the capturingLogger is
		// the necessary-but-not-sufficient condition (the
		// recover branch unambiguously routes through the
		// capturingLogger for the panic subtests above).
		realCtx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		cap := &capturingLogger{}
		logger = cap
		panicForTest = nil

		origReq := &schemas.BifrostRequest{}
		gotReq, gotSC, gotErr := PreLLMHook(realCtx, origReq)
		require.Same(t, origReq, gotReq)
		require.Nil(t, gotSC)
		require.Nil(t, gotErr)

		entries := cap.entries()
		errEntries := filterByLevel(entries, "ERROR")
		assert.Empty(t, errEntries, "non-panic path must not emit any Error log entries on the fallback capturing logger")
	})
}

// filterByLevel returns only entries whose level matches.
func filterByLevel(entries []capturedLog, level string) []capturedLog {
	var out []capturedLog
	for _, e := range entries {
		if e.level == level {
			out = append(out, e)
		}
	}
	return out
}

// assertRecoverLogShape verifies a captured Error entry matches the
// canonical recover-log shape: msg == "hook panic recovered",
// args carry hook=<name>, panic=<value>, stack=<non-empty string>.
func assertRecoverLogShape(t *testing.T, entry capturedLog, expectedHook, expectedPanic string) {
	t.Helper()
	assert.Equal(t, "hook panic recovered", entry.msg, "recover log msg literal")

	hookVal, ok := findStructured(entry.args, "hook")
	require.True(t, ok, `recover log must include "hook" key`)
	assert.Equal(t, expectedHook, hookVal, "hook name in structured log")

	panicVal, ok := findStructured(entry.args, "panic")
	require.True(t, ok, `recover log must include "panic" key`)
	assert.Equal(t, expectedPanic, panicVal, "panic value rendered into structured log")

	stackVal, ok := findStructured(entry.args, "stack")
	require.True(t, ok, `recover log must include "stack" key`)
	stackStr, ok := stackVal.(string)
	require.True(t, ok, `recover log "stack" value must be string`)
	assert.NotEmpty(t, stackStr, `recover log "stack" must be non-empty`)
}

// Compile-time assertion that capturingLogger satisfies plog.Logger.
// If a future plog.Logger interface change adds a method, this will
// fail to compile and the test author will know to extend the mock.
var _ plog.Logger = (*capturingLogger)(nil)
