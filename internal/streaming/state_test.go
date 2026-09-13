package streaming

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// TestState_OnChunk locks the METER-04 unified-detector per-chunk
// accumulator behavior per 03-03-PLAN.md <behavior>. Covers:
//
//   - nil chunk -> no panic, no state change
//   - non-chat variant -> ignored (BifrostChatResponse == nil short-circuits)
//   - first chunk -> FirstChunkAt + LastChunkAt set; TotalChunks=1
//   - sequential chunks -> FirstChunkAt preserved; LastChunkAt advances
//   - last-wins usage harvest -> final Usage value overwrites earlier ones
func TestState_OnChunk(t *testing.T) {
	t.Run("nil chunk — no panic, no state change", func(t *testing.T) {
		s := New()
		require.NotPanics(t, func() { s.OnChunk(nil) })
		require.Zero(t, s.Metrics.TotalChunks)
		require.True(t, s.Metrics.FirstChunkAt.IsZero())
		require.True(t, s.Metrics.LastChunkAt.IsZero())
		require.Zero(t, s.Metrics.InputTokens)
		require.Zero(t, s.Metrics.OutputTokens)
		require.Zero(t, s.Metrics.TotalTokens)
	})

	t.Run("non-chat variant (error-only) — TotalChunks unchanged", func(t *testing.T) {
		s := New()
		// BifrostStreamChunk with only the error variant populated — Bifrost
		// uses this to signal a transport/provider error mid-stream.
		errChunk := &schemas.BifrostStreamChunk{
			BifrostError: &schemas.BifrostError{},
		}
		s.OnChunk(errChunk)
		require.Zero(t, s.Metrics.TotalChunks,
			"non-chat variant must NOT contribute to chat streaming metrics (Research §2)")
		require.True(t, s.Metrics.FirstChunkAt.IsZero())
	})

	t.Run("first chat chunk — timestamps + TotalChunks=1", func(t *testing.T) {
		s := New()
		before := time.Now()
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{ID: "x"},
		}
		s.OnChunk(chunk)
		after := time.Now()

		require.Equal(t, 1, s.Metrics.TotalChunks)
		require.False(t, s.Metrics.FirstChunkAt.IsZero(), "first chunk MUST set FirstChunkAt")
		// Within tolerance of [before, after].
		require.WithinDuration(t, before, s.Metrics.FirstChunkAt, time.Second)
		require.WithinDuration(t, after, s.Metrics.FirstChunkAt, time.Second)
		// LastChunkAt equals FirstChunkAt on the first chunk.
		require.Equal(t, s.Metrics.FirstChunkAt, s.Metrics.LastChunkAt)
		// No usage on first chunk -> tokens still zero.
		require.Zero(t, s.Metrics.InputTokens)
		require.Zero(t, s.Metrics.OutputTokens)
		require.Zero(t, s.Metrics.TotalTokens)
	})

	t.Run("sequential chunks, no usage — FirstChunkAt preserved, LastChunkAt advances", func(t *testing.T) {
		s := New()
		// Fire 5 chunks; sleep tiny amounts so LastChunkAt advances measurably.
		for i := 0; i < 5; i++ {
			s.OnChunk(&schemas.BifrostStreamChunk{
				BifrostChatResponse: &schemas.BifrostChatResponse{ID: "x"},
			})
			time.Sleep(2 * time.Millisecond)
		}
		require.Equal(t, 5, s.Metrics.TotalChunks)
		require.False(t, s.Metrics.FirstChunkAt.IsZero())
		// LastChunkAt > FirstChunkAt because chunks 2..5 advanced it.
		require.True(t, s.Metrics.LastChunkAt.After(s.Metrics.FirstChunkAt),
			"LastChunkAt must advance past FirstChunkAt across sequential chunks")
		require.Zero(t, s.Metrics.InputTokens)
		require.Zero(t, s.Metrics.OutputTokens)
		require.Zero(t, s.Metrics.TotalTokens)
	})

	t.Run("terminal usage harvest — last-wins overwrites partials", func(t *testing.T) {
		s := New()
		// 3 chunks with no usage.
		for i := 0; i < 3; i++ {
			s.OnChunk(&schemas.BifrostStreamChunk{
				BifrostChatResponse: &schemas.BifrostChatResponse{ID: "x"},
			})
		}
		// Chunk 4: usage = (10, 5, 15).
		s.OnChunk(&schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				ID: "x",
				Usage: &schemas.BifrostLLMUsage{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
				},
			},
		})
		// Chunk 5: usage = (10, 8, 18) — final, overwrites chunk 4 per last-wins.
		s.OnChunk(&schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				ID: "x",
				Usage: &schemas.BifrostLLMUsage{
					PromptTokens:     10,
					CompletionTokens: 8,
					TotalTokens:      18,
				},
			},
		})

		require.Equal(t, 5, s.Metrics.TotalChunks)
		require.Equal(t, 10, s.Metrics.InputTokens,
			"last-wins: final Usage.PromptTokens overrides earlier partial")
		require.Equal(t, 8, s.Metrics.OutputTokens,
			"last-wins: final Usage.CompletionTokens overrides earlier partial")
		require.Equal(t, 18, s.Metrics.TotalTokens,
			"last-wins: final Usage.TotalTokens overrides earlier partial")
	})
}

// TestState_ModelProvider locks the streaming model+provider fold-in: the
// terminal chat chunk's Model + ExtraFields.Provider are threaded into the
// accumulator with last-wins-on-non-empty semantics (mirrors the token
// harvest), so BuildStream can emit a non-blank model/provider the real
// Revenium API requires.
func TestState_ModelProvider(t *testing.T) {
	t.Run("terminal chat chunk model+provider captured", func(t *testing.T) {
		s := New()
		s.OnChunk(&schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				ID:    "x",
				Model: "claude-3-5-sonnet",
				ExtraFields: schemas.BifrostResponseExtraFields{
					Provider: "anthropic",
				},
			},
		})
		m := s.Finalize()
		require.Equal(t, "claude-3-5-sonnet", m.Model,
			"terminal chat chunk Model MUST thread into StreamMetrics.Model")
		require.Equal(t, "anthropic", m.Provider,
			"terminal chat chunk ExtraFields.Provider MUST thread into StreamMetrics.Provider")
	})

	t.Run("last-wins-on-non-empty — later non-empty value wins, empty does NOT clobber", func(t *testing.T) {
		s := New()
		// First chunk carries model+provider.
		s.OnChunk(&schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				ID:    "x",
				Model: "gpt-4o",
				ExtraFields: schemas.BifrostResponseExtraFields{
					Provider: "openai",
				},
			},
		})
		// A subsequent chunk carries NO model/provider — must not clobber.
		s.OnChunk(&schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{ID: "x"},
		})
		m := s.Finalize()
		require.Equal(t, "gpt-4o", m.Model,
			"empty model on a later chunk MUST NOT clobber the earlier captured value")
		require.Equal(t, "openai", m.Provider,
			"empty provider on a later chunk MUST NOT clobber the earlier captured value")
	})
}

// TestState_Finalize is a thin regression to guard against accidental
// finalization-side-effect introduction. Finalize must remain a pure read of
// Metrics for the OnChunk last-wins semantics to be observable by the
// terminal-chunk branch of HTTPTransportStreamChunkHook.
func TestState_Finalize(t *testing.T) {
	s := New()
	s.Metrics.FirstChunkAt = time.Unix(1700000000, 0)
	s.Metrics.LastChunkAt = time.Unix(1700000100, 0)
	s.Metrics.TotalChunks = 7
	s.Metrics.InputTokens = 1
	s.Metrics.OutputTokens = 2
	s.Metrics.TotalTokens = 3

	got := s.Finalize()
	require.Equal(t, s.Metrics, got, "Finalize MUST return Metrics verbatim")
}
