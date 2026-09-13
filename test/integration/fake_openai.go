//go:build integration

// fake_openai.go is the integration-suite's fake OpenAI-shaped HTTP server.
// Lifted verbatim from spike/contract/fake_openai.go with two changes:
//
//  1. Build tag switched from `//go:build spike` to `//go:build integration`
//     so this file only compiles into the test/integration sub-module test
//     binary (the production .so does not need a mock provider; only the
//     integration driver does).
//  2. Added `GetCallCount() int` accessor (NOT in the spike) that returns the
//     number of /v1/chat/completions POSTs received so far. Tests use this to
//     prove the BUDGET-03 short-circuit reaches the wire as a real 429
//     WITHOUT falling through to the provider (D-15.4 evidence at the wire
//     level — the Phase 2 D-21 deferred assertion lands here in Plan 04-03).
//
// Three response modes (matching the spike verbatim):
//   - normal JSON completion
//   - streaming SSE chunks terminated by `data: [DONE]\n\n`
//   - 500 error (triggered by a `"model"` value containing `-error`)
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"
)

// openaiCallCount is the atomic counter incremented at the TOP of handleChat
// before any branching, so that even a malformed body increments the count.
// This is the conservative shape — the test asserts EXACT-EQUAL deltas
// across scenarios; an "only count on a 200" shape would let a 4xx-from-bad-
// body silently mask a provider-fallback regression.
var openaiCallCount atomic.Int64

// StartFakeOpenAI spins up an httptest.Server that serves OpenAI-shaped
// /v1/chat/completions responses in three modes:
//   - normal JSON completion
//   - streaming SSE chunks terminated by `data: [DONE]\n\n`
//   - 500 error (triggered by a `"model"` value containing `-error`)
//
// Returns the live base URL plus a shutdown closure. The caller is
// responsible for invoking shutdown in test teardown.
func StartFakeOpenAI() (string, func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	srv := httptest.NewServer(mux)
	return srv.URL, srv.Close
}

// GetCallCount returns the current /v1/chat/completions POST count, atomic-
// loaded. The integration suite captures an initialOpenAICalls value before
// each test scenario and asserts on the post-scenario delta — see
// TestWire_BudgetBlocked429NoFallback for the no-provider-fallback assertion.
func GetCallCount() int {
	return int(openaiCallCount.Load())
}

// handleChat decodes the inbound JSON body and dispatches to one of the
// three response modes. Decoded body is `map[string]any` for shape
// introspection — we only peek at `stream` and `model`.
func handleChat(w http.ResponseWriter, r *http.Request) {
	// Increment FIRST, before any decode/branch. This is the conservative
	// counter shape: any inbound POST to /v1/chat/completions counts as a
	// provider call from the integration suite's perspective, regardless of
	// what the body looks like or how we respond.
	openaiCallCount.Add(1)

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"decode failed: %v"}}`, err), http.StatusBadRequest)
		return
	}

	streaming := false
	if v, ok := body["stream"].(bool); ok {
		streaming = v
	}

	modelErr := false
	if v, ok := body["model"].(string); ok && strings.Contains(v, "-error") {
		modelErr = true
	}

	switch {
	case modelErr:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"integration-error","type":"server_error"}}`))
	case streaming:
		writeStreamingResponse(w)
	default:
		writeNormalResponse(w)
	}
}

// writeNormalResponse writes a standard OpenAI chat-completion response.
func writeNormalResponse(w http.ResponseWriter) {
	resp := map[string]any{
		"id":      "chatcmpl-integration-1",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "gpt-4o-mini",
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "hello from fake"},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 5, "total_tokens": 9},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeStreamingResponse writes three content deltas + a usage chunk +
// `data: [DONE]`. A 10ms sleep between chunks gives Bifrost's
// HTTPTransportStreamChunkHook time to fire per chunk so the streaming
// roll-up's TTFT measurement is non-zero (METER-04 requires TTFT > 0).
func writeStreamingResponse(w http.ResponseWriter) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	created := time.Now().Unix()
	words := []string{"hello", " from", " fake"}
	for _, word := range words {
		chunk := map[string]any{
			"id":      "chatcmpl-integration-1",
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         map[string]any{"content": word},
					"finish_reason": nil,
				},
			},
		}
		bs, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", bs)
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
	}

	// Final usage chunk with finish_reason=stop.
	usageChunk := map[string]any{
		"id":      "chatcmpl-integration-1",
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   "gpt-4o-mini",
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7},
	}
	bs, _ := json.Marshal(usageChunk)
	fmt.Fprintf(w, "data: %s\n\n", bs)
	flusher.Flush()

	// Terminal sentinel.
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
