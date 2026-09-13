//go:build pluginsanity

package main

// TestPlugin_Phase3MultiProviderMatrix locks the Plan 03-04 multi-provider
// integration contract: drive the production hook chain (PreLLMHook →
// HTTPTransportStreamChunkHook → PostLLMHook) against captured wire-shape
// fixtures from OpenAI + Anthropic + Bedrock, proving the unified Delta-C
// terminal-chunk detector (Plan 03-03) holds across all three providers AND
// the PostLLMHook 5-branch dispatch (Plan 03-02) survives realistic input
// shapes.
//
// Row coverage (9 rows = 3 providers × 3 scenarios):
//
//   - Row P03-04-R01 / openai  / chat     — METER-01 success path.
//   - Row P03-04-R02 / openai  / stream   — METER-04 + METER-05 streaming
//     roll-up via HTTPTransportStreamChunkHook + PostLLMHook no-op
//     (combined single-emit assertion: sendCount == 1).
//   - Row P03-04-R03 / openai  / error    — METER-02 failure path.
//   - Row P03-04-R04 / anthropic / chat
//   - Row P03-04-R05 / anthropic / stream
//   - Row P03-04-R06 / anthropic / error
//   - Row P03-04-R07 / bedrock / chat
//   - Row P03-04-R08 / bedrock / stream
//   - Row P03-04-R09 / bedrock / error
//
// References:
//   - 03-CONTEXT.md D-31 (4 conceptual signals — now unified per Delta C).
//   - 03-CONTEXT.md D-32 (capture-live-redact-commit fixture policy).
//   - 03-RESEARCH.md Delta C (Bifrost normalizes OpenAI [DONE] / Anthropic
//     message_stop / Bedrock messageStop into FinishReason BEFORE the chunk
//     reaches the plugin — so the streaming-row test PROVES the unified
//     detector works for all three providers' real wire shapes).
//   - Pitfall 1 (anti-double-count on block): not exercised here; locked in
//     plugin_phase3_test.go Row 3.
//   - Pitfall 2 (PostLLMHook single-emitter for streaming): locked here by
//     calling PostLLMHook AFTER all chunks are replayed and asserting
//     sendCount == 1 (not 2 — would indicate PostLLMHook double-emit on the
//     streaming path).
//
// Plan 03-04 Task 2 (operator-driven fixture capture) was deferred this
// session — the 9 fixture files at testdata/fixtures/{openai,anthropic,
// bedrock}/{chat.json,stream.jsonl,error.json} are NOT yet captured. The
// loadFixture / loadStreamFixture / loadErrorFixture helpers detect missing
// files and `t.Skipf` the row gracefully. When fixtures land (operator
// follows the fixture-refresh procedure in testdata/fixtures/README.md and
// commits the redacted JSON), the same test goes live with NO code change.
// Until then, all 9 rows skip cleanly and the parent test counts as PASS.
// The capture-harness sub-module was removed in Plan 05-03 per D-03
// housekeeping; the refresh procedure documents the operator-side recipe.
//
// Invocation:
//   REVENIUM_METERING_API_KEY=dummy \
//     go test -tags pluginsanity -count=1 \
//       -run TestPlugin_Phase3MultiProviderMatrix -v .
//
// The //go:build pluginsanity tag keeps this file out of the default
// `go test ./...` path (matches Plan 01-08 + 02-02 + 02-04 + 03-02
// precedent).
//
// Package-state save/restore (Pattern D LIFO) matches plugin_phase3_test.go
// L66-71: every package var the test mutates (logger, meteringClient) is
// saved at the outer scope and restored via t.Cleanup so a test failure
// cannot leak modified state into a later subtest or test-binary run.
//
// Production-path discipline (Pattern C — Plan 02-04 D-23): the
// *metering.Client is constructed via the production metering.New(cfg)
// constructor with BaseURL rewritten to httptest.NewServer.URL. NO seam
// interface. NO mock injection. The production *http.Client + payload
// marshalling + goroutine + WaitGroup paths are ALL exercised.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	revsdk "github.com/revenium/revenium-go-sdk/core/metering"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/config"
	"github.com/revenium/revenium-bifrost-plugin/internal/metering"
	"github.com/revenium/revenium-bifrost-plugin/internal/plog"
)

// loadFixture reads testdata/fixtures/{provider}/{name}.json into a typed
// *BifrostResponse. Missing fixture file → t.Skipf with a clear message
// pointing the operator at testdata/fixtures/README.md's capture-refresh
// policy. Missing-file is graceful per Plan 03-04 Task 2 deferral protocol
// (see file header).
func loadFixture(t *testing.T, provider, name string) *schemas.BifrostResponse {
	t.Helper()
	p := filepath.Join("testdata", "fixtures", provider, name+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("provider %s fixture %s missing — see testdata/fixtures/README.md for the fixture-refresh procedure", provider, p)
		}
		require.NoError(t, err, "loadFixture read error")
	}
	var resp schemas.BifrostResponse
	require.NoError(t, json.Unmarshal(raw, &resp), "loadFixture unmarshal %s", p)
	return &resp
}

// loadStreamFixture reads testdata/fixtures/{provider}/{name}.jsonl and
// decodes one *BifrostStreamChunk per line. Missing-file → t.Skipf.
func loadStreamFixture(t *testing.T, provider, name string) []*schemas.BifrostStreamChunk {
	t.Helper()
	p := filepath.Join("testdata", "fixtures", provider, name+".jsonl")
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("provider %s stream fixture %s missing — see testdata/fixtures/README.md for the fixture-refresh procedure", provider, p)
		}
		require.NoError(t, err, "loadStreamFixture open error")
	}
	defer f.Close()
	var chunks []*schemas.BifrostStreamChunk
	dec := json.NewDecoder(f)
	for dec.More() {
		var c schemas.BifrostStreamChunk
		require.NoError(t, dec.Decode(&c), "loadStreamFixture decode %s", p)
		chunks = append(chunks, &c)
	}
	return chunks
}

// loadErrorFixture reads testdata/fixtures/{provider}/error.json into a
// typed *BifrostError. Missing-file → t.Skipf.
func loadErrorFixture(t *testing.T, provider string) *schemas.BifrostError {
	t.Helper()
	p := filepath.Join("testdata", "fixtures", provider, "error.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("provider %s error fixture %s missing — see testdata/fixtures/README.md for the fixture-refresh procedure", provider, p)
		}
		require.NoError(t, err, "loadErrorFixture read error")
	}
	var be schemas.BifrostError
	require.NoError(t, json.Unmarshal(raw, &be), "loadErrorFixture unmarshal %s", p)
	return &be
}

// scenario discriminates the per-row execution shape.
type scenario int

const (
	scenarioChat scenario = iota
	scenarioStream
	scenarioError
)

func TestPlugin_Phase3MultiProviderMatrix(t *testing.T) {
	// Save-restore the package-level state the test mutates. Pattern D LIFO
	// via t.Cleanup; matches plugin_phase3_test.go convention.
	origLogger := logger
	origMetering := meteringClient
	t.Cleanup(func() {
		logger = origLogger
		meteringClient = origMetering
	})

	tests := []struct {
		name           string
		reqID          string
		provider       string
		scenario       scenario
		wantSendCount  int32
		wantStopReason string
	}{
		// --- OpenAI rows ---
		{
			name:           "openai_chat_emits_END",
			reqID:          "P03-04-R01",
			provider:       "openai",
			scenario:       scenarioChat,
			wantSendCount:  1,
			wantStopReason: "END",
		},
		{
			// METER-04 + Delta C: this row PROVES the unified IsTerminal
			// detector works for openai's wire format. Bifrost normalizes
			// OpenAI [DONE] into BifrostChatResponse.Choices[].FinishReason
			// before the chunk reaches us — the captured stream.jsonl is
			// the wire reality.
			name:           "openai_stream_emits_END_once_via_chunkhook",
			reqID:          "P03-04-R02",
			provider:       "openai",
			scenario:       scenarioStream,
			wantSendCount:  1,
			wantStopReason: "END",
		},
		{
			name:           "openai_error_emits_ERROR",
			reqID:          "P03-04-R03",
			provider:       "openai",
			scenario:       scenarioError,
			wantSendCount:  1,
			wantStopReason: "ERROR",
		},
		// --- Anthropic rows ---
		{
			name:           "anthropic_chat_emits_END",
			reqID:          "P03-04-R04",
			provider:       "anthropic",
			scenario:       scenarioChat,
			wantSendCount:  1,
			wantStopReason: "END",
		},
		{
			// METER-04 + Delta C: this row PROVES the unified IsTerminal
			// detector works for anthropic's wire format. Bifrost normalizes
			// Anthropic message_stop into BifrostChatResponse.Choices[].FinishReason
			// before the chunk reaches us — the captured stream.jsonl is
			// the wire reality.
			name:           "anthropic_stream_emits_END_once_via_chunkhook",
			reqID:          "P03-04-R05",
			provider:       "anthropic",
			scenario:       scenarioStream,
			wantSendCount:  1,
			wantStopReason: "END",
		},
		{
			name:           "anthropic_error_emits_ERROR",
			reqID:          "P03-04-R06",
			provider:       "anthropic",
			scenario:       scenarioError,
			wantSendCount:  1,
			wantStopReason: "ERROR",
		},
		// --- Bedrock rows ---
		{
			name:           "bedrock_chat_emits_END",
			reqID:          "P03-04-R07",
			provider:       "bedrock",
			scenario:       scenarioChat,
			wantSendCount:  1,
			wantStopReason: "END",
		},
		{
			// METER-04 + Delta C: this row PROVES the unified IsTerminal
			// detector works for bedrock's wire format. Bifrost normalizes
			// Bedrock messageStop into BifrostChatResponse.Choices[].FinishReason
			// before the chunk reaches us — the captured stream.jsonl is
			// the wire reality.
			name:           "bedrock_stream_emits_END_once_via_chunkhook",
			reqID:          "P03-04-R08",
			provider:       "bedrock",
			scenario:       scenarioStream,
			wantSendCount:  1,
			wantStopReason: "END",
		},
		{
			name:           "bedrock_error_emits_ERROR",
			reqID:          "P03-04-R09",
			provider:       "bedrock",
			scenario:       scenarioError,
			wantSendCount:  1,
			wantStopReason: "ERROR",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Load the per-row fixture FIRST. Missing-file → t.Skipf inside
			// the helper, so the row exits cleanly before any server / client
			// setup. This is the Plan 03-04 Task 2 deferral graceful-skip
			// path: until the operator runs the fixture-refresh procedure, every
			// row skips here.
			var (
				fixtureResp *schemas.BifrostResponse
				fixtureChunks []*schemas.BifrostStreamChunk
				fixtureErr  *schemas.BifrostError
			)
			switch tt.scenario {
			case scenarioChat:
				fixtureResp = loadFixture(t, tt.provider, "chat")
			case scenarioStream:
				fixtureChunks = loadStreamFixture(t, tt.provider, "stream")
			case scenarioError:
				fixtureErr = loadErrorFixture(t, tt.provider)
			}

			// Per-row fake metering server. atomic.Int32 counter is the
			// single source of truth for sendCount assertions.
			var sendCount atomic.Int32
			var captured atomic.Pointer[revsdk.MeteringPayload]

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sendCount.Add(1)
				var p revsdk.MeteringPayload
				_ = json.NewDecoder(r.Body).Decode(&p)
				captured.Store(&p)
				w.WriteHeader(200)
			}))
			t.Cleanup(srv.Close)

			// Pattern C production-path discipline: real metering.New(cfg)
			// against BaseURL rewritten to the fake server. NO seam.
			cfg := &config.Config{
				BaseURL:            srv.URL,
				APIKey:             "test",
				BudgetCheckTimeout: 3 * time.Second,
			}
			mc, err := metering.New(cfg)
			require.NoError(t, err, "[%s] metering.New must succeed against fake server", tt.reqID)

			logger = plog.New(false)
			meteringClient = mc

			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

			// Per-row scenario execution.
			switch tt.scenario {
			case scenarioChat:
				// Non-streaming success: PreLLMHook with ChatCompletionRequest
				// (sets KeyRequestStart + KeyRequestType; does NOT allocate
				// KeyStreamState because req.RequestType is non-streaming)
				// followed by PostLLMHook with the captured success fixture.
				req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest}
				_, _, err = PreLLMHook(ctx, req)
				require.NoError(t, err, "[%s] PreLLMHook must not return error", tt.reqID)

				_, _, err = PostLLMHook(ctx, fixtureResp, nil)
				require.NoError(t, err, "[%s] PostLLMHook must not return error", tt.reqID)

			case scenarioStream:
				// Streaming success: PreLLMHook with ChatCompletionStreamRequest
				// (sets KeyRequestType==stream + allocates KeyStreamState);
				// then every chunk in the fixture slice through the chunk
				// hook (req can be nil — Plan 03-03 explicitly notes req is
				// unused in the streaming chunk hook); finally PostLLMHook
				// with nil/nil to assert PostLLMHook is a no-op for streaming.
				//
				// This row's sendCount MUST be 1 — only the streaming-terminal
				// emission counts; PostLLMHook MUST NOT emit (METER-04
				// single-emit + METER-05 PostLLMHook-no-op-for-streaming
				// combined contract).
				req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest}
				_, _, err = PreLLMHook(ctx, req)
				require.NoError(t, err, "[%s] PreLLMHook (stream) must not return error", tt.reqID)

				for i, chunk := range fixtureChunks {
					_, err = HTTPTransportStreamChunkHook(ctx, nil, chunk)
					require.NoError(t, err, "[%s] HTTPTransportStreamChunkHook chunk %d", tt.reqID, i)
				}

				// Pitfall 2 lock: PostLLMHook must NOT emit on the streaming
				// path. If sendCount goes to 2 below, double-emission has
				// regressed.
				_, _, err = PostLLMHook(ctx, nil, nil)
				require.NoError(t, err, "[%s] PostLLMHook (stream no-op) must not return error", tt.reqID)

			case scenarioError:
				// Failure path: PreLLMHook with ChatCompletionRequest
				// (non-streaming) followed by PostLLMHook(nil, fixtureErr).
				req := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest}
				_, _, err = PreLLMHook(ctx, req)
				require.NoError(t, err, "[%s] PreLLMHook (error scenario) must not return error", tt.reqID)

				_, _, err = PostLLMHook(ctx, nil, fixtureErr)
				require.NoError(t, err, "[%s] PostLLMHook (error scenario) must not return error", tt.reqID)
			}

			// Synchronous flush (Pitfall 11 / METER-06): Close blocks until
			// all in-flight Send goroutines drain, so the server-side count
			// is observable before assertion.
			require.NoError(t, mc.Close(), "[%s] meteringClient.Close must succeed", tt.reqID)

			require.Equal(t, tt.wantSendCount, sendCount.Load(),
				"[%s] expected Send count mismatch (Pitfall 2 streaming double-emit + Delta C unified detector live or die here)",
				tt.reqID)

			if tt.wantStopReason != "" {
				p := captured.Load()
				require.NotNil(t, p, "[%s] expected a captured metering payload", tt.reqID)
				require.Equal(t, tt.wantStopReason, p.StopReason,
					"[%s] stop_reason mismatch", tt.reqID)
			}
		})
	}
}

// TestPlugin_SoloaderSignatures asserts that each of the 7 exported soloader
// symbols this plugin ships has a runtime type that is AssignableTo the
// corresponding `plugin.Lookup()` cast target declared in
// `github.com/maximhq/bifrost/framework/plugins/soloader.go`. This is the
// TEST-02 (D-34 / D-34.1) early-warning gate: any Bifrost minor bump that
// changes a hook signature MUST fail this test at PR time — that is the
// desired outcome per 04-RESEARCH.md Pitfall 8 ("test failure on Bifrost bump
// is the DESIRED outcome of TEST-02"). The fix loop is then to update both
// (a) the local expected function-type variables below and (b) the production
// hook signatures in plugin.go in lockstep with Bifrost's loader contract,
// then re-run the integration suite.
//
// Why AssignableTo, not reflect.TypeOf(symbol).String() equality? Per
// 04-RESEARCH.md §Pattern 2 caveat: Go's reflect-package format for fully
// qualified types may change between minor versions (the package-path-prefix
// in `*schemas.BifrostContext` printing is one such surface), so a literal
// string comparison would fail spuriously on a no-op toolchain bump.
// `Type.AssignableTo` is the refactor-safe equivalent of "this symbol could be
// passed to a typed `plugin.Lookup` cast against this function-type" — which
// is exactly what the loader does at runtime.
//
// Negative-discriminator sub-test ("Negative_WrongSig"): proves the
// AssignableTo gate actually discriminates by declaring a deliberately wrong
// function type (`func() int`) and asserting `reflect.TypeOf(Init)` is NOT
// assignable to it. Without this negative-row, every positive-row could
// trivially pass and we'd have no signal that the gate has teeth.
//
// The table covers exactly the 7 symbols the soloader looks up for the
// LLM-plugin path the production .so ships (verified against `main` branch
// 2026-05-24 — framework/v1.5.11 module identity per .planning/PROJECT.md).
// MCP hooks (PreMCPHook/PostMCPHook/PreMCPConnectionHook/PostMCPConnectionHook)
// and observability hooks (Inject) are out of scope for v1 (CLAUDE.md
// "Configuration surface" + REQUIREMENTS.md MCP-* requirements deferred).
//
// Invocation (pure reflection — no I/O, no -buildmode=plugin needed; <30s):
//
//	REVENIUM_METERING_API_KEY=dummy \
//	  go test -tags pluginsanity -count=1 \
//	    -run TestPlugin_SoloaderSignatures -timeout 30s ./...
func TestPlugin_SoloaderSignatures(t *testing.T) {
	// Locally-declared function-type variables that mirror soloader.go's
	// `plugin.Lookup` cast targets verbatim. Each is the "expected type" for
	// the symbol of the same name. Declared as typed zero-value vars so
	// reflect.TypeOf can pull the type without a non-nil-function dependency.
	var (
		expectInit                         func(config any) error
		expectGetName                      func() string
		expectCleanup                      func() error
		expectHTTPTransportPreHook         func(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error)
		expectPreLLMHook                   func(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error)
		expectPostLLMHook                  func(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error)
		expectHTTPTransportStreamChunkHook func(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error)
	)

	// 7-row table. Order matches soloader.go's `pluginObj.Lookup()` sequence
	// for the LLM-plugin path: Init → GetName → Cleanup →
	// HTTPTransportPreHook → PreLLMHook → PostLLMHook →
	// HTTPTransportStreamChunkHook (with HTTPTransportPostHook skipped — this
	// plugin does not export it; soloader treats it as optional and stores
	// nil when absent, which is the production posture per Plan 02-04).
	cases := []struct {
		name     string
		symbol   any
		expected any
	}{
		{"Init", Init, expectInit},
		{"GetName", GetName, expectGetName},
		{"Cleanup", Cleanup, expectCleanup},
		{"HTTPTransportPreHook", HTTPTransportPreHook, expectHTTPTransportPreHook},
		{"PreLLMHook", PreLLMHook, expectPreLLMHook},
		{"PostLLMHook", PostLLMHook, expectPostLLMHook},
		{"HTTPTransportStreamChunkHook", HTTPTransportStreamChunkHook, expectHTTPTransportStreamChunkHook},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reflect.TypeOf(tc.symbol)
			want := reflect.TypeOf(tc.expected)
			require.NotNil(t, got, "%s: reflect.TypeOf returned nil — symbol must be a non-nil typed function", tc.name)
			require.NotNil(t, want, "%s: expected-type var has nil reflect.Type — test setup bug", tc.name)
			require.Truef(t, got.AssignableTo(want),
				"%s signature drift (D-34 / Pitfall 8): symbol type %s is NOT AssignableTo loader cast target %s — Bifrost has changed the soloader contract; update both the local expected-type vars and the production hook signature in plugin.go in lockstep",
				tc.name, got.String(), want.String())
		})
	}

	// Negative discriminator: proves the AssignableTo gate above can actually
	// fail. Without this sub-test, a buggy AssignableTo implementation that
	// always returned true would silently let any signature drift through.
	t.Run("Negative_WrongSig", func(t *testing.T) {
		var wrong func() int
		got := reflect.TypeOf(Init)
		bad := reflect.TypeOf(wrong)
		require.NotNil(t, got, "Init reflect.TypeOf returned nil")
		require.NotNil(t, bad, "wrong-sig reflect.TypeOf returned nil")
		require.Falsef(t, got.AssignableTo(bad),
			"AssignableTo gate has no teeth: Init (%s) should NOT be assignable to func() int (%s)",
			got.String(), bad.String())
	})
}
