// Package plog is the unified logging surface for the Revenium Bifrost
// plugin. It bridges two distinct logging paths into a single Logger
// interface:
//
//   - Inside a hook body (PreLLMHook / PostLLMHook /
//     HTTPTransportStreamChunkHook), log lines flow through
//     *schemas.BifrostContext.Log(level, msg) so they appear in Bifrost's
//     per-request log stream with correct level filtering.
//   - In Init / Cleanup / background goroutines (where no
//     *schemas.BifrostContext exists), log lines flow through stdlib
//     log/slog with a JSON handler writing to stderr.
//
// FromCtx(ctx, fallback) is the single bridge call site — every package in
// this module should obtain its Logger via FromCtx so the right backing
// implementation is selected automatically.
//
// Bifrost's ctx.Log signature is Log(level LogLevel, msg string) — it does
// NOT accept key/value pairs. ctxLogger.emit folds args into the msg suffix
// as " key=value key=value..." via foldArgs so per-request logs remain
// searchable in the Bifrost log stream. fmt.Sprint is used for value
// stringification so nil ctx.Value() results and time.Time values
// (request-start timestamps from PreLLMHook) all serialize cleanly without
// special-casing — see the Blocker 4 fix discussion on foldArgs.
//
// Per LOG-03, any sensitive value (e.g., key hash) must be truncated to its
// first 8 characters before being passed to a logger; TruncateKeyHash is the
// canonical helper.
package plog

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// Logger is the unified interface every package logs through. Implementations
// are slogLogger (for Init/Cleanup/background) and ctxLogger (for hook bodies
// that hold a *schemas.BifrostContext).
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// New constructs a package-level slog-backed Logger writing JSON to stderr.
// When debug is true the handler emits LevelDebug and above; otherwise only
// LevelInfo and above. Used in Init to populate the package-level logger var
// that Cleanup, recoverHook, and any background paths read from.
func New(debug bool) Logger {
	lvl := slog.LevelInfo
	if debug {
		lvl = slog.LevelDebug
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return &slogLogger{l: slog.New(h)}
}

// FromCtx returns a ctx-bridged Logger when ctx is non-nil — i.e., we are
// inside a hook body and the Bifrost host injected a per-request plugin
// scope. When ctx is nil (Init / Cleanup / background goroutine) FromCtx
// returns the fallback Logger unchanged so its output reaches stderr.
func FromCtx(ctx *schemas.BifrostContext, fallback Logger) Logger {
	if ctx == nil {
		return fallback
	}
	return &ctxLogger{ctx: ctx, fallback: fallback}
}

// TruncateKeyHash returns the first 8 characters of h, or h itself when it
// has 8 or fewer characters. Per LOG-03 (and the LiteLLM ReveniumGuardrail's
// key_hash[:8] discipline) this is the only sanctioned way to render a key
// hash in a log line.
func TruncateKeyHash(h string) string {
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

// --- slogLogger ---

// slogLogger wraps a *slog.Logger so it satisfies the Logger interface
// without exposing slog-specific types to callers.
type slogLogger struct{ l *slog.Logger }

func (s *slogLogger) Debug(msg string, args ...any) { s.l.Debug(msg, args...) }
func (s *slogLogger) Info(msg string, args ...any)  { s.l.Info(msg, args...) }
func (s *slogLogger) Warn(msg string, args ...any)  { s.l.Warn(msg, args...) }
func (s *slogLogger) Error(msg string, args ...any) { s.l.Error(msg, args...) }

// --- ctxLogger ---

// ctxLogger bridges Logger calls to *schemas.BifrostContext.Log. Bifrost's
// ctx.Log signature is (LogLevel, string) — no args — so we fold key/value
// pairs into the msg suffix via foldArgs before delegating. The fallback
// field is retained for symmetry with FromCtx but is not exercised in the
// emit path (Bifrost's ctx.Log silently no-ops outside plugin scope rather
// than returning an error we could detect to switch to the fallback).
type ctxLogger struct {
	ctx      *schemas.BifrostContext
	fallback Logger
}

func (c *ctxLogger) Debug(msg string, args ...any) { c.emit(schemas.LogLevelDebug, msg, args...) }
func (c *ctxLogger) Info(msg string, args ...any)  { c.emit(schemas.LogLevelInfo, msg, args...) }
func (c *ctxLogger) Warn(msg string, args ...any)  { c.emit(schemas.LogLevelWarn, msg, args...) }
func (c *ctxLogger) Error(msg string, args ...any) { c.emit(schemas.LogLevelError, msg, args...) }

func (c *ctxLogger) emit(level schemas.LogLevel, msg string, args ...any) {
	c.ctx.Log(level, foldArgs(msg, args...))
}

// foldArgs walks args in (key, value) pairs and appends them to msg as
// " key=value" entries. An odd-length args slice silently drops the trailing
// orphan key (matching slog's tolerant behavior on misuse).
//
// Stringification goes through fmt.Sprint, which:
//   - Renders untyped nil as "<nil>" — Phase 2/3 hook bodies frequently
//     call ctx.Value(ctxkeys.KeyHash) which returns any(nil) before the key
//     has been set; the resulting log line must not panic and must not
//     produce an empty value (Blocker 4 fix).
//   - Renders time.Time via its String() method — Phase 2/3 hook bodies
//     pass time.Now() and request-start timestamps; the canonical
//     "2006-01-02 15:04:05 +0000 UTC" form is the on-the-wire log contract
//     downstream parsers depend on (Blocker 4 fix).
//
// Any future "optimization" that special-cases types via a switch and
// bypasses fmt.Sprint MUST update the foldArgs unit tests to prove nil and
// time.Time still serialize correctly.
func foldArgs(msg string, args ...any) string {
	if len(args) < 2 {
		return msg
	}
	var sb strings.Builder
	sb.Grow(len(msg) + 16*(len(args)/2))
	sb.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		sb.WriteByte(' ')
		fmt.Fprint(&sb, args[i])
		sb.WriteByte('=')
		fmt.Fprint(&sb, args[i+1])
	}
	return sb.String()
}
