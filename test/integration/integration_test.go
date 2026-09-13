//go:build integration

// integration_test.go drives the //go:build integration wire-level suite for
// the production revenium-bifrost.so plugin against a source-built bin/
// bifrost-http subprocess. Lifts the spike/contract harness shape verbatim
// (findRepoRoot, findBifrostHTTP, pickFreePort, renderConfig, waitForBifrost)
// and refines the spike's TestMain into the wrapper-closure pattern from
// 04-RESEARCH.md §Pitfall 6 so a panic during setup still tears down the
// bifrost-http subprocess and the two fake servers.
//
// Plan-context:
//   - TEST-03 (REQUIREMENTS.md) — the 4 wire scenarios per D-33.1.
//   - D-21 (Phase 2 CONTEXT.md) — wire-level 429 + AllowFallbacks=false + no-
//     provider-fallback assertions were explicitly deferred to Plan 04-03;
//     TestWire_BudgetBlocked429NoFallback is where they land.
//   - D-33 (Phase 4 CONTEXT.md) — test/integration mirrors spike/contract's
//     sub-module shape verbatim, with D-04 isolation guaranteeing zero
//     production-internal imports (the assertion is structural: the parent
//     module's internal subtree is unreachable from a sibling sub-module).
//   - Pitfall 7 (04-RESEARCH.md) — the integration suite cleans up its OWN
//     bifrost-http subprocess inside runMain's defer so the CI step ordering
//     in Plan 04-04 (smoke-then-integration) doesn't depend on Bifrost's
//     supervisor for cleanup.
//
// Test naming discipline: keyHash values are distinct per test so no between-
// test reset is required (the fake_revenium.go state map is keyed on keyHash;
// distinct keys keep state simple and tests independent).
//
// Run locally (after `make build && make bifrost-host`):
//
//	cd test/integration && go test -tags integration -count=1 -timeout 180s -v .
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"
)

// Package-level test state populated by runMain. All TestWire_* tests read
// these fields; only runMain mutates them. Single-threaded mutation
// discipline — no atomic / mutex needed because m.Run() blocks until all
// tests complete and the defers fire only after Run returns.
var (
	bifrostBaseURL      string
	bifrostProc         *os.Process
	bifrostLogPath      string
	fakeOpenAIURL       string
	fakeReveniumURL     string
	getReveniumPayloads func() []map[string]any
	setBudgetBlock      func(keyHash string, exceededCount int)
)

// findRepoRoot walks up from the test working directory until it finds the
// top-level `.planning/` directory (the main module's marker, NOT
// test/integration/go.mod). Used to resolve bin/bifrost-http and
// revenium-bifrost.so paths from a stable anchor. Lifted verbatim from
// spike/contract/contract_test.go.
func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".planning")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate repo root from %s", cwd)
		}
		dir = parent
	}
}

// findBifrostHTTP returns the absolute path of <repo-root>/bin/bifrost-http
// or an error if the binary is missing. Lifted verbatim from
// spike/contract/contract_test.go.
func findBifrostHTTP() (string, error) {
	root, err := findRepoRoot()
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "bin", "bifrost-http")
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// findReveniumSo locates the production-built plugin .so at the repo root.
// Differs from spike/contract/contract_test.go's findSpikeSo (which looked
// at spike/contract/spike.so) — Plan 04-03 deliberately loads the production
// .so the host runtime would, NOT a spike-tagged build.
func findReveniumSo() (string, error) {
	root, err := findRepoRoot()
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "revenium-bifrost.so")
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// pickFreePort asks the kernel for an unused port, closes the listener,
// and returns the port number. Lifted verbatim from spike/contract.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// renderConfig fills config.json.tmpl with the mock provider URL and the
// production .so absolute path. Lifted from spike/contract/contract_test.go's
// renderConfig with the second template var renamed SpikeSoPath → PluginSoPath
// (per Task 1's config.json.tmpl deltas).
func renderConfig(tmplPath, outPath, mockURL, pluginSo string) error {
	tmpl, err := template.ParseFiles(tmplPath)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create config.json: %w", err)
	}
	defer f.Close()
	return tmpl.Execute(f, struct {
		MockProviderURL string
		PluginSoPath    string
	}{MockProviderURL: mockURL, PluginSoPath: pluginSo})
}

// waitForBifrost polls the bifrost-http health endpoint until it responds
// or the deadline expires. Lifted verbatim from spike/contract.
func waitForBifrost(baseURL string, deadline time.Time) error {
	client := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/api/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		resp, err = client.Get(baseURL + "/")
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("bifrost-http did not become healthy before %s", deadline.Format(time.RFC3339))
}

// TestMain is the wrapper-closure entry point per 04-RESEARCH.md §Pitfall 6.
// The actual setup + m.Run() + teardown lives inside runMain so a panic in
// setup still runs the deferred teardown (SIGTERM bifrostProc, close fake
// servers, close log file).
//
// Reference: golang/go #37206 + #34129 — `os.Exit` skips deferred funcs by
// Go contract; the wrapper closure pattern defers teardown inside runMain
// and the outer TestMain calls `os.Exit` AFTER runMain returns.
func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

// runMain encapsulates TestMain's body so deferred teardown ALWAYS runs,
// including on panic. Required so a panic in TestMain's setup (e.g., a
// bifrost-http startup failure) still tears the subprocess down — at matrix
// scale, leaked processes accumulate fast (Pitfall 7).
func runMain(m *testing.M) (code int) {
	bifrostHTTP, err := findBifrostHTTP()
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: bin/bifrost-http missing — run 'make bifrost-host' first")
		return 0 // skip cleanly — matches spike/contract's exit-0-on-missing-artifact contract
	}
	pluginSo, err := findReveniumSo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration: revenium-bifrost.so missing — run 'make build' first")
		return 0
	}

	// Fake-OpenAI server (httptest); shutdown closure stored in defer below.
	mockURL, mockShutdown := StartFakeOpenAI()
	fakeOpenAIURL = mockURL

	// Fake-Revenium server (httptest); same shutdown discipline.
	revURL, getPayloads, blockFn, revShutdown := StartFakeRevenium()
	fakeReveniumURL = revURL
	getReveniumPayloads = getPayloads
	setBudgetBlock = blockFn

	tmpDir, err := os.MkdirTemp("", "revenium-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: mkdir temp: %v\n", err)
		revShutdown()
		mockShutdown()
		return 1
	}

	configPath := filepath.Join(tmpDir, "config.json")
	if err := renderConfig("config.json.tmpl", configPath, mockURL, pluginSo); err != nil {
		fmt.Fprintf(os.Stderr, "integration: render config: %v\n", err)
		revShutdown()
		mockShutdown()
		return 1
	}

	bifrostLogPath = filepath.Join(tmpDir, "bifrost.log")

	port, err := pickFreePort()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: pick port: %v\n", err)
		revShutdown()
		mockShutdown()
		return 1
	}
	bifrostBaseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	logFile, err := os.Create(bifrostLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: create bifrost.log: %v\n", err)
		revShutdown()
		mockShutdown()
		return 1
	}

	cmd := exec.Command(bifrostHTTP,
		"-port", strconv.Itoa(port),
		"-app-dir", tmpDir,
		"-log-level", "debug",
		"-log-style", "json",
	)
	// REVENIUM_METERING_BASE_URL points the production plugin at the fake
	// Revenium server; REVENIUM_METERING_API_KEY is a non-empty sentinel so
	// config.Load()'s required-vars check passes. NO SPIKE_TRACE_LOG — that
	// var was honored only by spike.go; the production plugin (plugin.go)
	// does not consume it.
	cmd.Env = append(os.Environ(),
		"REVENIUM_METERING_BASE_URL="+revURL,
		"REVENIUM_METERING_API_KEY=integration-fake-key",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "integration: start bifrost-http: %v\n", err)
		_ = logFile.Close()
		revShutdown()
		mockShutdown()
		return 1
	}
	bifrostProc = cmd.Process

	// Defer teardown — runs ON ANY EXIT FROM runMain (return, panic). The
	// outer TestMain calls os.Exit AFTER runMain returns, so this is the
	// last opportunity to clean up before the process truly exits.
	// SIGTERM + 8s grace + Kill fallback mirrors spike/contract's pattern.
	defer func() {
		if bifrostProc != nil {
			_ = bifrostProc.Signal(syscall.SIGTERM)
		}
		exited := make(chan struct{})
		go func() {
			_, _ = cmd.Process.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(8 * time.Second):
			_ = bifrostProc.Kill()
		}
		_ = logFile.Close()
		revShutdown()
		mockShutdown()
	}()

	if err := waitForBifrost(bifrostBaseURL, time.Now().Add(10*time.Second)); err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		// Dump bifrost.log to stderr so the matrix runner's logs show why
		// startup failed. Mirrors the spike's failure-dump pattern.
		_ = logFile.Sync()
		if logBytes, readErr := os.ReadFile(bifrostLogPath); readErr == nil {
			fmt.Fprintln(os.Stderr, "--- bifrost.log ---")
			fmt.Fprintln(os.Stderr, string(logBytes))
			fmt.Fprintln(os.Stderr, "--- end bifrost.log ---")
		}
		return 1
	}

	return m.Run()
}

// postChatRaw issues a POST to /v1/chat/completions with custom headers. The
// caller is responsible for closing the body.
func postChatRaw(t *testing.T, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, bifrostBaseURL+"/v1/chat/completions", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// waitForPayloadCount polls until len(getReveniumPayloads()) reaches the
// target count or the deadline expires. Required because revenium-go-sdk's
// MeteringClient.Send is fire-and-forget (goroutine + circuit breaker); the
// POST arrives some time after the synchronous response body returns. A
// fixed sleep would either be too short (flaky on slow runners) or too long
// (wastes 4 × 200ms per test). Polling at 25ms intervals is responsive
// enough for the matrix.
func waitForPayloadCount(t *testing.T, target int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(getReveniumPayloads()) >= target {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	// Fall through — let the caller's require.Equal produce the diff.
}

// TestWire_BudgetBlocked429NoFallback (TEST-03 (a), D-33.1, Phase 2 D-21
// landing site) — Bifrost short-circuits with a wire 429 and does NOT fall
// through to the upstream provider when PreLLMHook returns a
// LLMPluginShortCircuit with StatusCode=&429 and AllowFallbacks=&false. The
// 429 body is the BUDGET-03 structured detail (`error.budgets[0].risk =
// "EXCEEDED"`) verified at internal/budget/shortcircuit_test.go.
func TestWire_BudgetBlocked429NoFallback(t *testing.T) {
	const keyHash = "block-no-fallback"
	setBudgetBlock(keyHash, 1) // any positive integer triggers exceededCount > 0

	initialOpenAICalls := GetCallCount()

	resp := postChatRaw(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":false}`, map[string]string{
		"x-revenium-key-hash": keyHash,
	})
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	require.Equal(t, 429, resp.StatusCode, "expected 429 short-circuit; got %d body=%s", resp.StatusCode, respBody)

	// The 429 detail body has the BUDGET-03 structured shape (Plan 02-03 +
	// internal/budget/shortcircuit.go lock):
	//   {"error":{"message":"Budget exceeded","type":"budget_exceeded",
	//    "guardrail":"revenium","budgets":[{"name":"daily","risk":"EXCEEDED",...}]}}
	// Bifrost wraps the plugin's BifrostError into its own envelope and
	// JSON-escapes the inner body (verified empirically 2026-05-24: outer
	// wire body has shape `{"is_bifrost_error":false,"status_code":429,
	// "error":{"message":"{\"error\":...\"risk\":\"EXCEEDED\"...}"}}`).
	// The substring-grep accepts either the unescaped form OR the JSON-
	// escaped form so the assertion is tolerant of future Bifrost envelope
	// shape evolution while still pinning the BUDGET-03 EXCEEDED-risk
	// signal at the wire level.
	bodyStr := string(respBody)
	hasUnescaped := bytes.Contains(respBody, []byte(`"risk":"EXCEEDED"`))
	hasEscaped := bytes.Contains(respBody, []byte(`\"risk\":\"EXCEEDED\"`))
	require.True(t, hasUnescaped || hasEscaped,
		"429 body must contain the BUDGET-03 risk=EXCEEDED structured detail (either form); got %s", bodyStr)

	// Give Bifrost a moment to drain post-short-circuit hooks before reading
	// the provider call count. Mirrors the spike's 200ms wait.
	time.Sleep(200 * time.Millisecond)

	deltaCalls := GetCallCount() - initialOpenAICalls
	require.Equal(t, 0, deltaCalls,
		"no upstream provider fallback expected on AllowFallbacks=false short-circuit (D-15.4 wire-level evidence; Phase 2 D-21 deferred)")
}

// TestWire_AllowedNonStreamingOneSuccessEvent (TEST-03 (b)) — allowed non-
// streaming request emits exactly ONE metering POST with stopReason="END"
// and middlewareSource="GUARDRAIL" (METER-01). Field names are camelCase
// per revenium-go-sdk core/metering/payload.go json tags (verified
// 2026-05-24).
func TestWire_AllowedNonStreamingOneSuccessEvent(t *testing.T) {
	const keyHash = "allowed-non-stream"
	// setBudgetBlock is intentionally NOT called for this keyHash — default
	// exceededCount=0 → allow path.

	initialPayloads := len(getReveniumPayloads())
	initialOpenAICalls := GetCallCount()

	resp := postChatRaw(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":false}`, map[string]string{
		"x-revenium-key-hash": keyHash,
	})
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	require.Equal(t, 200, resp.StatusCode, "expected 200 from fake-OpenAI; got %d body=%s", resp.StatusCode, respBody)
	require.Contains(t, string(respBody), "chatcmpl-integration-1", "expected fake-OpenAI body; got %s", respBody)
	require.Equal(t, 1, GetCallCount()-initialOpenAICalls, "exactly one upstream provider call expected for allowed non-streaming request")

	// MeteringClient.Send is fire-and-forget; the POST arrives shortly after
	// the response body returns. Sleep modestly to let the goroutine flush.
	waitForPayloadCount(t, initialPayloads+1, 2*time.Second)

	payloads := getReveniumPayloads()
	require.Equal(t, initialPayloads+1, len(payloads), "exactly ONE metering event expected for allowed non-streaming request (METER-01)")

	newPayload := payloads[len(payloads)-1]
	require.Equal(t, "END", newPayload["stopReason"], "stopReason MUST be END for successful non-streaming completion (METER-01)")
	require.Equal(t, "GUARDRAIL", newPayload["middlewareSource"], "middlewareSource MUST be GUARDRAIL constant (Pitfall 13 / D-13)")
}

// TestWire_StreamingOneEventTTFTAndTokens (TEST-03 (c)) — streaming request
// emits exactly ONE metering POST with isStreamed=true, timeToFirstToken>0,
// and totalTokenCount>0. Asserts METER-04 + METER-05 (single event from the
// HTTPTransportStreamChunkHook terminal-chunk branch; NOT one from
// PostLLMHook + one from StreamChunkHook).
//
// Numeric JSON fields decode to float64 via map[string]any — the assertions
// compare against float64(0) accordingly. The actual server-side values are
// int64 in revenium-go-sdk's MeteringPayload struct; JSON marshaling/
// unmarshaling round-trips them through float64 in the untyped-decode path.
func TestWire_StreamingOneEventTTFTAndTokens(t *testing.T) {
	const keyHash = "allowed-streaming"
	initialPayloads := len(getReveniumPayloads())

	resp := postChatRaw(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":true}`, map[string]string{
		"x-revenium-key-hash": keyHash,
	})
	streamBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.Equal(t, 200, resp.StatusCode, "expected 200 from streaming endpoint; got %d", resp.StatusCode)
	require.Contains(t, string(streamBody), "data: [DONE]", "stream body MUST contain the OpenAI terminal sentinel; got %s", streamBody)

	// Streaming roll-up emits AFTER the stream terminator per Plan 03-03
	// contract. fake_openai.go sleeps 10ms between chunks (~30ms total
	// streaming time), so a 2s wait is a generous upper bound for the
	// terminal-chunk-hook flush.
	waitForPayloadCount(t, initialPayloads+1, 2*time.Second)

	payloads := getReveniumPayloads()
	require.Equal(t, initialPayloads+1, len(payloads),
		"exactly ONE metering event expected from streaming roll-up (METER-04 + METER-05: NOT one from PostLLMHook + one from StreamChunkHook)")

	newPayload := payloads[len(payloads)-1]
	require.Equal(t, true, newPayload["isStreamed"], "isStreamed MUST be true on streaming roll-up (METER-04)")
	require.Equal(t, "GUARDRAIL", newPayload["middlewareSource"], "middlewareSource MUST be GUARDRAIL (Pitfall 13)")

	ttft, ok := newPayload["timeToFirstToken"].(float64)
	require.True(t, ok, "timeToFirstToken MUST be present and numeric; got %v (type %T)", newPayload["timeToFirstToken"], newPayload["timeToFirstToken"])
	require.Greater(t, ttft, float64(0), "timeToFirstToken MUST be > 0 on streaming completion (METER-04)")

	totalTokens, ok := newPayload["totalTokenCount"].(float64)
	require.True(t, ok, "totalTokenCount MUST be present and numeric; got %v (type %T)", newPayload["totalTokenCount"], newPayload["totalTokenCount"])
	require.Greater(t, totalTokens, float64(0), "totalTokenCount MUST be > 0 on streaming completion (METER-05)")
}

// TestWire_BlockedOneBudgetExceededEvent (TEST-03 (d), METER-03 anti-double-
// count) — blocked request emits exactly ONE metering POST with
// stopReason="BUDGET_EXCEEDED". The symmetry-guarantee test: Bifrost fires
// PostLLMHook even on PreLLMHook short-circuit (D-15.1 spike-verified), and
// the plugin's blocked builder emits the BUDGET_EXCEEDED event — NOT a
// zero-token success event and NOT a double event (one block + one zero-
// token success).
//
// keyHash distinct from TestWire_BudgetBlocked429NoFallback so the captured-
// payload assertions remain independent (no shared blocked-state).
func TestWire_BlockedOneBudgetExceededEvent(t *testing.T) {
	const keyHash = "block-and-meter"
	setBudgetBlock(keyHash, 1)

	initialPayloads := len(getReveniumPayloads())

	resp := postChatRaw(t, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"stream":false}`, map[string]string{
		"x-revenium-key-hash": keyHash,
	})
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	require.Equal(t, 429, resp.StatusCode, "expected 429 short-circuit; got %d body=%s", resp.StatusCode, respBody)

	// PostLLMHook fires AFTER short-circuit per Plan 01 D-15.1 + Plan 02's
	// blocked-event emission; the metering event is fire-and-forget. Wait
	// for it to arrive at fake-Revenium.
	waitForPayloadCount(t, initialPayloads+1, 2*time.Second)

	payloads := getReveniumPayloads()
	require.Equal(t, initialPayloads+1, len(payloads),
		"exactly ONE BUDGET_EXCEEDED metering event expected on block (METER-03 anti-double-count: NOT one block + one zero-token success)")

	newPayload := payloads[len(payloads)-1]
	require.Equal(t, "BUDGET_EXCEEDED", newPayload["stopReason"],
		"stopReason MUST be BUDGET_EXCEEDED on block (METER-03; the literal is NOT in SDK stop_reason.go constants — Revenium-specific)")
	require.Equal(t, "GUARDRAIL", newPayload["middlewareSource"], "middlewareSource MUST be GUARDRAIL (Pitfall 13)")
}

// Static interface checks — ensures the integration module's go.mod resolves
// the expected packages. Compiles to a no-op at runtime.
var (
	_ = json.Marshal
	_ = io.Copy
)
