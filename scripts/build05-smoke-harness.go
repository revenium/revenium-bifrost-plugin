//go:build ignore

// scripts/build05-smoke-harness.go — BUILD-05 per-runner plugin.Open smoke
// harness (Phase 4 D-35.2). Mirrors the Phase 1 spike/contract harness shape
// at the matrix granularity: starts a fake-OpenAI server, picks a free port,
// renders a minimal config.json that loads the freshly-built
// revenium-bifrost.so via bifrost-http, drives one chat-completion curl
// against the fake provider, SIGTERMs bifrost-http, and prints the
// SIGTERM-to-Cleanup delta + log path for the wrapping shell script to
// assert on.
//
// Why a separate harness from test/integration/: D-35.2 explicitly keeps
// BUILD-05 outside the integration sub-module — the integration suite is the
// deeper assertion (4 wire scenarios); BUILD-05 is the cheap per-runner
// toolchain/symbol drift detector that runs FIRST in ci.yml (Pitfall 7) so
// failure surfaces fast. The harness inlines a fake-OpenAI handler + a
// config.json template so it has zero cross-module imports (D-04 isolation
// honored in BOTH directions).
//
// 1-second floor (D-35.2): the Phase 1 spike captured a 2.115383s
// SIGTERM-to-Cleanup delta (D-16.5 evidence anchor at
// .planning/phases/01-foundation-bifrost-contract-spike/01-SPIKE-FINDINGS.md
// referencing the spike's deliberate time.Sleep(2*time.Second) inside its
// Cleanup). The production plugin's Cleanup does NOT sleep — it synchronously
// flushes the metering client + revenium-go-sdk's resilience pipeline, which
// typically takes ~50-500ms. A 1s floor is the safe minimum that still proves
// Bifrost honored the synchronous-Cleanup contract: any value below 1s
// suggests the process was killed mid-Cleanup (regression). Above 1s proves
// Bifrost waited for the plugin's Cleanup to return before tearing the
// process down. The wrapping shell script does the float comparison via awk.
//
// -trimpath is NOT required on this `go run` invocation: the harness loads
// the .so via `bifrost-http` SUBPROCESS (which itself was built with
// -trimpath via `make bifrost-host`), NOT via this process's stdlib `plugin`
// package. The `-trimpath` matching requirement from Phase 1 Plan 05 only
// applies when the loader process AND the .so are the same Go binary.
//
// Usage (invoked by scripts/build05-smoke.sh):
//
//	go run ./scripts/build05-smoke-harness.go \
//	  -bifrost-bin bin/bifrost-http \
//	  -plugin-so revenium-bifrost.so

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"text/template"
	"time"
)

// Minimal config.json shape — verified-correct keys are `client` / `providers`
// / `plugins` per Plan 01-06 Warning 12. Plugin name MUST be the literal
// "revenium" — the production plugin's GetName() returns this exact string
// (verified at plugin.go in main module). Mirrors test/integration/config.json.tmpl
// and spike/contract/config.json.tmpl (those are the canonical references) but
// inlined here for D-04 isolation — the harness imports NOTHING from sibling
// sub-modules.
const configTmpl = `{
  "client": {
    "drop_excess_requests": false,
    "initial_pool_size": 1
  },
  "providers": {
    "openai": {
      "keys": [{"name": "smoke-default", "value": "smoke-fake-openai-key", "models": ["*"], "weight": 1.0}],
      "network_config": {
        "base_url": "{{.MockProviderURL}}"
      }
    }
  },
  "plugins": [
    {
      "enabled": true,
      "name": "revenium",
      "path": "{{.PluginSoPath}}"
    }
  ]
}
`

// startFakeOpenAI inlines a minimal OpenAI chat-completion handler. BUILD-05
// only needs ONE successful chat completion to drive PreLLMHook → PostLLMHook
// — no streaming, no error mode, no per-model branching needed (the deeper
// scenarios live in test/integration/ + spike/contract/).
func startFakeOpenAI() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Drain body so the client gets a clean read result even if Bifrost
		// proxies the body verbatim.
		_, _ = io.Copy(io.Discard, r.Body)
		resp := map[string]any{
			"id":      "chatcmpl-build05-smoke-1",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o-mini",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "hello from build05 smoke"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

// pickFreePort asks the kernel for an unused port. Mirrors
// spike/contract/contract_test.go::pickFreePort + Pitfall 7 (each step picks
// its own free port even when run serially).
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

func renderConfig(outPath, mockURL, pluginSo string) error {
	tmpl, err := template.New("config").Parse(configTmpl)
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

// failf prints a clear error to stderr and exits 1. The wrapping shell
// script greps stdout for telemetry lines; setup failures go to stderr so
// they don't accidentally look like successful telemetry to the grep gates.
func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "build05-smoke-harness: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	var (
		bifrostBin  string
		pluginSo    string
		timeoutSecs int
		outputDir   string
	)
	flag.StringVar(&bifrostBin, "bifrost-bin", "bin/bifrost-http", "path to bifrost-http binary")
	flag.StringVar(&pluginSo, "plugin-so", "revenium-bifrost.so", "path to revenium-bifrost .so")
	flag.IntVar(&timeoutSecs, "timeout-secs", 30, "overall harness timeout in seconds")
	flag.StringVar(&outputDir, "output-dir", "", "output dir for config.json + bifrost.log (default: os.MkdirTemp)")
	flag.Parse()

	// Resolve absolute paths so the bifrost-http subprocess sees them
	// regardless of its working directory choices.
	absBifrostBin, err := filepath.Abs(bifrostBin)
	if err != nil {
		failf("resolve bifrost-bin abs: %v", err)
	}
	if _, err := os.Stat(absBifrostBin); err != nil {
		failf("bifrost-bin %q not found (run `make bifrost-host`): %v", absBifrostBin, err)
	}
	absPluginSo, err := filepath.Abs(pluginSo)
	if err != nil {
		failf("resolve plugin-so abs: %v", err)
	}
	if _, err := os.Stat(absPluginSo); err != nil {
		failf("plugin-so %q not found (run `make build`): %v", absPluginSo, err)
	}

	if outputDir == "" {
		outputDir, err = os.MkdirTemp("", "build05-smoke-*")
		if err != nil {
			failf("mkdir temp: %v", err)
		}
	} else {
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			failf("mkdir output-dir %q: %v", outputDir, err)
		}
	}

	// 1) Start fake OpenAI server.
	fake := startFakeOpenAI()
	defer fake.Close()

	// 2) Pick a free port for bifrost-http.
	port, err := pickFreePort()
	if err != nil {
		failf("pick free port: %v", err)
	}
	bifrostBaseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	// 3) Render config.json with the mock provider URL + absolute plugin path.
	configPath := filepath.Join(outputDir, "config.json")
	if err := renderConfig(configPath, fake.URL, absPluginSo); err != nil {
		failf("render config: %v", err)
	}

	// 4) Open bifrost.log so we can capture stdout+stderr for the wrapping
	//    shell script's grep gates.
	bifrostLogPath := filepath.Join(outputDir, "bifrost.log")
	logFile, err := os.Create(bifrostLogPath)
	if err != nil {
		failf("create bifrost.log: %v", err)
	}
	defer logFile.Close()

	// 5) Spawn bifrost-http with the plugin loaded. Env vars per BUILD-05
	//    spec: REVENIUM_METERING_BASE_URL points at a non-routing address
	//    (127.0.0.1:9 is reserved/discard) so the plugin's metering Send
	//    fails gracefully (logged at WARN); the smoke does NOT need a real
	//    metering target — only to prove plugin.Open + Init + PreLLMHook +
	//    Cleanup happened.
	cmd := exec.Command(absBifrostBin,
		"-port", strconv.Itoa(port),
		"-app-dir", outputDir,
		"-log-level", "debug",
		"-log-style", "json",
	)
	cmd.Env = append(os.Environ(),
		"REVENIUM_METERING_BASE_URL=http://127.0.0.1:9",
		"REVENIUM_METERING_API_KEY=smoke-fake-key",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		failf("start bifrost-http: %v", err)
	}

	// 6) Poll readiness with a 60s deadline. On failure, dump bifrost.log to
	//    stderr so the CI runner log shows what went wrong.
	//
	//    Why 60s and not 10s: on a cold start bifrost-http performs a BLOCKING
	//    model-catalog network sync before it serves health. On the CI
	//    `macos-15-intel` runner this was observed downloading 4,661 pricing
	//    records and 12,247 model-parameter records at T+4s/T+5s, which leaves
	//    no margin inside a 10s budget — the harness failed deterministically
	//    there (reproduced on an isolated re-run), not as a flake. waitForBifrost
	//    polls in a loop and returns the instant health succeeds, so a longer
	//    deadline costs nothing on a fast runner; it only buys headroom on a
	//    slow, cold one.
	if err := waitForBifrost(bifrostBaseURL, time.Now().Add(60*time.Second)); err != nil {
		_ = logFile.Sync()
		if b, readErr := os.ReadFile(bifrostLogPath); readErr == nil {
			fmt.Fprintln(os.Stderr, "--- bifrost.log (startup failure) ---")
			fmt.Fprintln(os.Stderr, string(b))
			fmt.Fprintln(os.Stderr, "--- end bifrost.log ---")
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
		failf("waitForBifrost: %v", err)
	}

	// 7) Drive ONE chat-completion POST against bifrost-http (stream=false).
	//    The `x-revenium-key-hash` header is intentionally a noop value — the
	//    plugin's PreLLMHook code path runs regardless; we just need ONE
	//    PreLLMHook log line.
	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"build05 smoke"}],"stream":false}`
	req, err := http.NewRequest(http.MethodPost,
		bifrostBaseURL+"/v1/chat/completions",
		bytes.NewBufferString(reqBody))
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
		failf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-revenium-key-hash", "smoke-noop")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
		failf("POST /v1/chat/completions: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// 8) Give PostLLMHook a beat to fire (mirrors spike/contract pattern).
	time.Sleep(200 * time.Millisecond)

	// 9) Capture SIGTERM timestamp RIGHT before signaling so the delta we
	//    compute is conservative (never understates the wait).
	sigTermSentAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_, _ = cmd.Process.Wait()
		failf("send SIGTERM: %v", err)
	}

	// 10) Wait for the process to exit. 15s ceiling; Bifrost's synchronous
	//     Cleanup contract should return in well under that for the
	//     production plugin (no deliberate sleep), but the full Bifrost
	//     shutdown (graceful HTTP server drain + per-plugin Cleanup chain +
	//     metering client resilience flush against an unreachable target)
	//     empirically takes ~7s on darwin/arm64 — well within 15s.
	//
	//     We MUST drive exit detection via cmd.Wait() in a goroutine + a
	//     `done` channel, NOT via Signal(0) polling. `Signal(0)` returns nil
	//     against a ZOMBIE process (state between exit and reap); only after
	//     Wait() does ESRCH surface. Polling Signal(0) without a Wait() reaper
	//     creates a deadlock: the process exits, we never observe ESRCH, the
	//     deadline elapses, and the test reports a false timeout.
	//     Mirrors spike/contract/contract_test.go lines 240-252 verbatim.
	//
	//     On timeout, escalate to Kill so the runner doesn't leak the
	//     subprocess (T-04-04-01 mitigation).
	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()

	var processExitedAt time.Time
	select {
	case <-exited:
		processExitedAt = time.Now()
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		// Reap after Kill to avoid leaving a zombie.
		<-exited
		// Surface telemetry so the wrapping shell script's assertions still
		// see a value (the assertion will fail on delta >= 1s, signaling
		// the SIGTERM contract is broken).
		fmt.Printf("BIFROST_LOG: %s\n", bifrostLogPath)
		fmt.Printf("SIGTERM_SENT_AT: %s\n", sigTermSentAt.UTC().Format(time.RFC3339Nano))
		fmt.Println("PROCESS_EXITED_AT: TIMEOUT")
		fmt.Println("DELTA_SECONDS: -1")
		failf("bifrost-http did not exit within 15s of SIGTERM — Cleanup did NOT return; process killed")
	}

	// 11) Flush bifrost.log so the shell script's grep gates see the final
	//     buffered lines (PostLLMHook + Cleanup typically land right at
	//     teardown).
	_ = logFile.Sync()

	delta := processExitedAt.Sub(sigTermSentAt)

	// 12) Emit telemetry lines on STDOUT for the wrapping shell script to
	//     parse. One line per key; exact strings (`BIFROST_LOG:`,
	//     `SIGTERM_SENT_AT:`, `PROCESS_EXITED_AT:`, `DELTA_SECONDS:`) are the
	//     contract.
	fmt.Printf("BIFROST_LOG: %s\n", bifrostLogPath)
	fmt.Printf("SIGTERM_SENT_AT: %s\n", sigTermSentAt.UTC().Format(time.RFC3339Nano))
	fmt.Printf("PROCESS_EXITED_AT: %s\n", processExitedAt.UTC().Format(time.RFC3339Nano))
	// Print with 6 decimal places of microsecond precision — the shell
	// script's awk comparison only needs >= 1.0, but the precision is useful
	// for SUMMARY-level evidence anchors.
	fmt.Printf("DELTA_SECONDS: %.6f\n", delta.Seconds())

	// Lightweight defense-in-depth: surface a hint if the delta is suspicious
	// even though the FINAL assertion happens in the shell script.
	if delta < time.Second {
		fmt.Fprintf(os.Stderr,
			"build05-smoke-harness: WARNING — SIGTERM-to-exit delta %s is < 1s floor (D-35.2); "+
				"Cleanup may NOT have synchronously flushed.\n", delta)
	}

}
