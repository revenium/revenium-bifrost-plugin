package streaming

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// ptrString returns a pointer to the given string — helper for constructing
// *string-typed FinishReason fixtures in table-driven cases below.
func ptrString(s string) *string { return &s }

// TestIsTerminal locks the Delta C unified-detector behavior per
// 03-03-PLAN.md <behavior> + 03-RESEARCH.md §2. Two signals only:
//   - Signal 1: chunk.BifrostError != nil (provider-agnostic)
//   - Signal 2: any chunk.BifrostChatResponse.Choices[*].FinishReason is
//     non-nil AND non-empty-string (unified post-normalization marker)
func TestIsTerminal(t *testing.T) {
	t.Run("nil chunk — false", func(t *testing.T) {
		require.False(t, IsTerminal(nil))
	})

	t.Run("empty chunk — no variants set, false", func(t *testing.T) {
		require.False(t, IsTerminal(&schemas.BifrostStreamChunk{}))
	})

	t.Run("Signal 1 — BifrostError set, provider-agnostic terminal", func(t *testing.T) {
		chunk := &schemas.BifrostStreamChunk{
			BifrostError: &schemas.BifrostError{},
		}
		require.True(t, IsTerminal(chunk),
			"any non-nil BifrostError is terminal regardless of provider")
	})

	t.Run("Signal 2 — chat with FinishReason='stop' is terminal", func(t *testing.T) {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{
					{FinishReason: ptrString("stop")},
				},
			},
		}
		require.True(t, IsTerminal(chunk),
			"FinishReason='stop' is the unified post-normalization terminal marker")
	})

	t.Run("Signal 2 — chat with FinishReason==nil is mid-stream (false)", func(t *testing.T) {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{
					{FinishReason: nil},
				},
			},
		}
		require.False(t, IsTerminal(chunk),
			"nil FinishReason means the stream is still in flight")
	})

	t.Run("Signal 2 — chat with FinishReason==ptr(\"\") is NOT terminal", func(t *testing.T) {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{
					{FinishReason: ptrString("")},
				},
			},
		}
		require.False(t, IsTerminal(chunk),
			"empty-string pointer FinishReason treated as 'not yet finished'")
	})

	t.Run("Signal 2 — chat with no Choices (nil slice) is NOT terminal", func(t *testing.T) {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: nil,
			},
		}
		require.False(t, IsTerminal(chunk))
	})

	t.Run("Signal 2 — chat with empty Choices slice is NOT terminal", func(t *testing.T) {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{},
			},
		}
		require.False(t, IsTerminal(chunk))
	})

	t.Run("Signal 2 — multi-choice ANY FinishReason set is terminal", func(t *testing.T) {
		chunk := &schemas.BifrostStreamChunk{
			BifrostChatResponse: &schemas.BifrostChatResponse{
				Choices: []schemas.BifrostResponseChoice{
					{FinishReason: nil},
					{FinishReason: ptrString("length")},
				},
			},
		}
		require.True(t, IsTerminal(chunk),
			"any non-nil/non-empty FinishReason on any choice is terminal")
	})
}
