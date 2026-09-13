package streaming

import "github.com/maximhq/bifrost/core/schemas"

// IsTerminal reports whether the given BifrostStreamChunk is the last chunk
// in a streamed completion — i.e., the chunk on which
// HTTPTransportStreamChunkHook must call Finalize and emit the roll-up via
// metering.BuildStream.
//
// Phase 3 implements a UNIFIED 2-branch detector per Delta C (03-RESEARCH.md
// §2). Bifrost normalizes OpenAI `data: [DONE]` (consumed in
// providers/utils/sse.go), Anthropic `message_stop` (translated in
// providers/anthropic/types.go:1447), and Bedrock `messageStop` (translated
// in providers/bedrock/responses.go:1729) INTO
// BifrostChatResponse.Choices[*].FinishReason BEFORE the chunk reaches this
// plugin. The detector returns true on either:
//
//  1. chunk.BifrostError != nil  — provider-agnostic; an error chunk ends
//     the stream regardless of provider.
//  2. any chunk.BifrostChatResponse.Choices[*].FinishReason is non-nil AND
//     non-empty-string — the unified terminal marker after Bifrost
//     normalization.
//
// CONTEXT.md D-31's "4 conceptual signals (OpenAI [DONE] / Anthropic
// message_stop / Bedrock messageStop / BifrostError)" framing was correct in
// spirit but wrong in implementation: only 2 framings reach the plugin
// because 3 of the 4 provider-specific markers are normalized inside the
// provider packages. The 3-provider fixture matrix in Plan 03-04 will PROVE
// the normalization holds end-to-end against captured live wire data.
//
// Some providers emit a usage-only final chunk AFTER the finish_reason chunk;
// we treat the finish_reason chunk as the cut-point to ensure exactly-once
// emission (the post-terminal usage chunk is still consumed by State.OnChunk
// via last-wins harvest before this hook fires for that chunk — but emission
// only happens here on the FIRST detected terminal chunk).
func IsTerminal(chunk *schemas.BifrostStreamChunk) bool {
	if chunk == nil {
		return false
	}
	// Signal 1: any provider error chunk is terminal — emit and stop. Provider-agnostic.
	if chunk.BifrostError != nil {
		return true
	}
	// Signal 2: chat-completion-stream terminal carries FinishReason on a choice.
	if chunk.BifrostChatResponse != nil {
		for _, choice := range chunk.BifrostChatResponse.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				return true
			}
		}
	}
	return false
}
