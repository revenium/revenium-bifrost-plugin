package plog

import (
	"reflect"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// fakeLogger is a sentinel Logger used to verify that FromCtx returns the
// fallback unchanged when ctx == nil. It records every method call so tests
// can also assert which level was invoked.
type fakeLogger struct {
	lastLevel string
	lastMsg   string
	lastArgs  []any
}

func (f *fakeLogger) Debug(msg string, args ...any) { f.lastLevel = "DEBUG"; f.lastMsg = msg; f.lastArgs = args }
func (f *fakeLogger) Info(msg string, args ...any)  { f.lastLevel = "INFO"; f.lastMsg = msg; f.lastArgs = args }
func (f *fakeLogger) Warn(msg string, args ...any)  { f.lastLevel = "WARN"; f.lastMsg = msg; f.lastArgs = args }
func (f *fakeLogger) Error(msg string, args ...any) { f.lastLevel = "ERROR"; f.lastMsg = msg; f.lastArgs = args }

// TestTruncateKeyHash locks in the LOG-03 contract: at most the first 8 chars
// of a key hash ever appear in a log line. Boundary cases (0, <8, =8, >8,
// realistic-sized) are all covered.
func TestTruncateKeyHash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"one char", "a", "a"},
		{"seven chars", "abcdefg", "abcdefg"},
		{"exactly eight chars", "abcdefgh", "abcdefgh"},
		{"nine chars", "abcdefghi", "abcdefgh"},
		{"realistic sk-* key", "sk-1234567890abcdef", "sk-12345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, TruncateKeyHash(tc.in))
		})
	}
}

// TestFromCtx_NilReturnsFallback proves that calling FromCtx(nil, fallback)
// returns the fallback Logger pointer-identical — never wraps it. This
// matters because Init/Cleanup/background goroutines pass ctx=nil and need
// the fallback to receive their log lines (otherwise the lines would vanish
// into a ctxLogger whose ctx.Log is a no-op outside plugin scope).
func TestFromCtx_NilReturnsFallback(t *testing.T) {
	fb := &fakeLogger{}
	got := FromCtx(nil, fb)
	require.Same(t, fb, got, "FromCtx(nil, fallback) must return fallback unchanged")
}

// TestFromCtx_NonNilReturnsBridge proves that calling FromCtx with a non-nil
// *BifrostContext returns a Logger whose underlying type is distinct from
// the fallback's type (i.e., the bridge wrapper was constructed). The
// emit-side behavior is exercised indirectly via TestFoldArgs and is not
// re-tested here because ctx.Log is a no-op outside plugin scope, so we
// can't observe its output from a unit test.
func TestFromCtx_NonNilReturnsBridge(t *testing.T) {
	fb := &fakeLogger{}
	got := FromCtx(&schemas.BifrostContext{}, fb)
	require.NotNil(t, got)
	require.NotEqual(
		t,
		reflect.TypeOf(fb),
		reflect.TypeOf(got),
		"FromCtx with non-nil ctx must return a bridge wrapper, not the fallback",
	)
}

// TestNew_DebugAndInfo asserts New returns a non-nil Logger for both debug
// settings and that the four level methods are panic-free. Output is
// directed to stderr in production; we don't intercept it here — the goal is
// to lock in the constructor contract.
func TestNew_DebugAndInfo(t *testing.T) {
	for _, debug := range []bool{false, true} {
		l := New(debug)
		require.NotNil(t, l, "New(%v) must return a non-nil Logger", debug)
		require.NotPanics(t, func() { l.Debug("dbg") })
		require.NotPanics(t, func() { l.Info("info") })
		require.NotPanics(t, func() { l.Warn("warn") })
		require.NotPanics(t, func() { l.Error("err") })
	}
}

// TestFoldArgs locks in the key=value formatting contract for ctxLogger.emit.
// Bifrost's ctx.Log(level, msg) takes a single string and no args, so plog
// folds key/value pairs into the msg suffix as " k1=v1 k2=v2".
//
// The nil-value, zero-time, and non-zero-time cases are the Blocker 4 fix:
// Phase 2/3 hook bodies routinely pass ctx.Value(KeyHash) (which is any(nil)
// when unset) and time.Now() into log lines. fmt.Sprint handles both cleanly
// (nil -> "<nil>", time.Time -> its String() form). Any future refactor that
// special-cases types via a switch statement instead of fmt.Sprint will
// break these tests immediately.
func TestFoldArgs(t *testing.T) {
	nonZero := time.Date(2026, 5, 20, 10, 15, 30, 0, time.UTC)
	cases := []struct {
		name string
		in   []any
		want string
	}{
		{"no args", []any{}, "hi"},
		{"string string", []any{"k", "v"}, "hi k=v"},
		{"string int", []any{"k", 42}, "hi k=42"},
		{"two pairs", []any{"a", 1, "b", 2}, "hi a=1 b=2"},
		{"orphan key", []any{"orphan"}, "hi"},
		// Blocker 4 — nil values from ctx.Value() must serialize cleanly.
		{"nil value", []any{"k", nil}, "hi k=<nil>"},
		// Blocker 4 — zero time.Time must use the canonical time.Time.String() form.
		{"zero time", []any{"t", time.Time{}}, "hi t=0001-01-01 00:00:00 +0000 UTC"},
		// Blocker 4 — non-zero time.Time locks in the exact format Phase 3 metering will emit.
		{"non-zero time", []any{"t", nonZero}, "hi t=2026-05-20 10:15:30 +0000 UTC"},
		// Combined sanity: a paired nil + paired time in the same call.
		{"mixed nil+time", []any{"k", nil, "t", time.Time{}}, "hi k=<nil> t=0001-01-01 00:00:00 +0000 UTC"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, foldArgs("hi", tc.in...))
		})
	}
}

// TestFoldArgs_TimeNowIsNonEmpty is the live-clock companion to the explicit
// time.Time cases above. time.Now() returns a value that's not stable across
// runs, so we assert the looser property that the resulting log line
// contains a non-empty value with at least one digit (i.e., real timestamp
// content was rendered, not "<nil>" or "").
func TestFoldArgs_TimeNowIsNonEmpty(t *testing.T) {
	got := foldArgs("hi", "t", time.Now().UTC())
	require.NotEqual(t, "hi", got, "time.Now() must produce a non-empty value, not be dropped")
	require.NotEqual(t, "hi t=<nil>", got, "time.Now() must NOT serialize as <nil>")
	require.Regexp(t, `^hi t=.*\d.*`, got)
}
