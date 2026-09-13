// Package streaming owns the per-request streaming accumulator (chunk count,
// first/last chunk timestamps, token totals) that the
// HTTPTransportStreamChunkHook builds up over the chunk stream and the
// terminal-chunk branch finalizes into a roll-up MeteringPayload via
// metering.BuildStream. Phase 1 ships the types + stub method bodies per D-09;
// Phase 3 implements OnChunk's per-chunk aggregation and Finalize's
// per-token-type roll-up.
package streaming

import (
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// StreamMetrics captures the roll-up of one streamed completion: timestamps
// for TTFT calculation (FirstChunkAt - request-start), total chunk count,
// final token totals harvested from the terminal usage chunk, and the
// model + provider sourced from the terminal chat chunk.
//
// Phase 3 populates the timing + token fields from BifrostStreamChunk
// inspection; the Model + Provider fields complete the deferred fold-in so
// the streaming roll-up emits a non-blank model — the real Revenium API
// rejects model="" with HTTP 400. Both are captured last-wins-on-non-empty
// in OnChunk (same harvest discipline as tokens) so the terminal chat chunk's
// values win without an empty later chunk clobbering them.
type StreamMetrics struct {
	FirstChunkAt time.Time
	LastChunkAt  time.Time
	TotalChunks  int
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	// Model is the model name from the terminal chat chunk
	// (BifrostChatResponse.Model), captured last-wins-on-non-empty.
	Model string
	// Provider is the provider attribution from the terminal chat chunk
	// (BifrostChatResponse.ExtraFields.Provider), captured
	// last-wins-on-non-empty.
	Provider string
}

// State is the mutable streaming accumulator stored under
// ctxkeys.KeyStreamState by PreLLMHook when the inbound request is detected as
// streaming, mutated by HTTPTransportStreamChunkHook on every chunk, and read
// by the terminal-chunk branch to emit the roll-up metering event.
//
// Phase 3 may add fields here (e.g., per-provider terminal signals, prompt
// tokens accumulated separately) — Phase 1's struct intentionally exports only
// Metrics so downstream code touches the accumulator via the OnChunk /
// Finalize methods rather than poking individual fields.
type State struct {
	Metrics StreamMetrics
	// additional Phase-3 fields reserved
}

// New returns an initialized *State with zero Metrics. Trivial in Phase 1; the
// constructor is exported so PreLLMHook (Phase 2) can `ctx.SetValue(KeyStreamState,
// streaming.New())` when stream detection succeeds.
func New() *State { return &State{} }

// OnChunk advances the accumulator with one BifrostStreamChunk. Phase 3
// implementation per 03-RESEARCH.md §2:
//
//  1. Nil-defense: silently return on nil chunk OR non-chat-variant chunk
//     (BifrostStreamChunk is a discriminated union of 8 variants — only the
//     BifrostChatResponse branch is in scope for chat-completion streaming
//     metrics; non-chat variants such as text/speech/transcription/error-only
//     chunks do not contribute to chat streaming roll-up).
//  2. Always-bump: increment TotalChunks for any in-scope chunk.
//  3. First-chunk timestamp: when FirstChunkAt is the zero time, set it to
//     now — this is the TTFT cut-point.
//  4. Last-chunk timestamp: always overwrite LastChunkAt with now.
//  5. Last-wins token harvest: when chunk.BifrostChatResponse.Usage is non-nil,
//     overwrite all three token totals. Some providers (per Bifrost's own
//     ProviderSendsDoneMarker logic, providers/utils/utils.go:2411) emit a
//     usage-only final chunk AFTER the finish_reason chunk; last-wins captures
//     it because OnChunk runs on every chunk including the post-terminal usage
//     chunk if the wire delivers one before the terminal-detect cut-point.
//
// Concurrency contract: Bifrost dispatches HTTPTransportStreamChunkHook
// serially per request — State methods are NOT mutex-guarded. If Bifrost
// changes this contract a sync.Mutex on State would be added; currently NOT
// required (verified Phase 1 spike D-15.3 ctx round-trip + 03-RESEARCH.md §3
// execution-order proof).
func (s *State) OnChunk(chunk *schemas.BifrostStreamChunk) {
	if chunk == nil || chunk.BifrostChatResponse == nil {
		return
	}
	s.Metrics.TotalChunks++
	now := time.Now()
	if s.Metrics.FirstChunkAt.IsZero() {
		s.Metrics.FirstChunkAt = now
	}
	s.Metrics.LastChunkAt = now
	if u := chunk.BifrostChatResponse.Usage; u != nil {
		s.Metrics.InputTokens = u.PromptTokens
		s.Metrics.OutputTokens = u.CompletionTokens
		s.Metrics.TotalTokens = u.TotalTokens
	}
	// Model + provider fold-in: last-wins-on-non-empty so the terminal chat
	// chunk's attribution wins without a later empty chunk clobbering it
	// (mirrors the token harvest). ExtraFields is a value field on
	// BifrostChatResponse per core@v1.5.11 — direct .Provider access, no
	// accessor. The real Revenium API rejects a blank model with HTTP 400, so
	// threading these is what lets streaming completions meter at all.
	if m := chunk.BifrostChatResponse.Model; m != "" {
		s.Metrics.Model = m
	}
	if p := string(chunk.BifrostChatResponse.ExtraFields.Provider); p != "" {
		s.Metrics.Provider = p
	}
}

// Finalize returns the accumulated StreamMetrics for the terminal-chunk branch
// of HTTPTransportStreamChunkHook to feed into metering.BuildStream. Phase 1
// returns the (zero) Metrics field directly; Phase 3 may add per-token-type
// finalization (e.g., reasoning vs. completion tokens) before returning.
func (s *State) Finalize() StreamMetrics { return s.Metrics }
