//go:build ignore

// smoke-plugin-open.go is a standalone Phase-1 verification harness that proves
// the Revenium Bifrost plugin .so loads cleanly via the stdlib plugin loader.
// It is NOT part of the normal build (the //go:build ignore tag above keeps
// `go build .` and `go test ./...` from picking it up). Run explicitly:
//
//	REVENIUM_METERING_API_KEY=spike-fake-key \
//	  go run -trimpath scripts/smoke-plugin-open.go ./revenium-bifrost.so
//
// IMPORTANT: -trimpath is REQUIRED on the `go run` invocation because the .so
// (per the Makefile `build` target) is built with -trimpath, and `plugin.Open`
// rejects loads when the host's build-flag set differs (it reports
// "plugin was built with a different version of package internal/goarch"
// even when the toolchain version + module versions all match). This is a
// stricter cousin of Pitfall 3 and is not separately documented in
// 01-RESEARCH.md; flagged in 01-05-SUMMARY.md for future maintainers.
//
// The harness mirrors Bifrost's framework/plugins/soloader.go cast pattern for
// the three Bifrost-typed-agnostic symbols (Init / GetName / Cleanup) — it
// asserts each plugin.Lookup returns a non-nil Symbol AND that the symbol
// type-asserts cleanly to the soloader-expected signature. The three hook
// symbols (PreLLMHook / PostLLMHook / HTTPTransportStreamChunkHook) are only
// asserted to EXIST via plugin.Lookup; their full type cast requires importing
// github.com/maximhq/bifrost/core/schemas which would couple this harness to
// the same Go-toolchain version the .so was built against — and that's exactly
// what the harness is trying to prove an independent way. Plan 06's spike
// driver does the full Bifrost-typed cast against a real bifrost-http binary.
//
// Exit codes:
//
//	0 — All six symbols present; Init / GetName / Cleanup casts succeed; GetName() == "revenium"
//	1 — Missing argv[1] (path to .so)
//	1 — plugin.Open failed (mismatched toolchain, missing symbols, OS/arch mismatch)
//	2 — A required symbol was missing from the .so
//	3 — A required symbol's type cast failed (Init/GetName/Cleanup signature drift)
//	4 — GetName() returned a string other than "revenium" (CONTRACT-03 violation)
//
// Required env var: REVENIUM_METERING_API_KEY (set defensively in case a future
// maintainer extends the harness to actually invoke Init — Init refuses to run
// without it per CONFIG-02). The harness as written does NOT call Init.
package main

import (
	"fmt"
	"os"
	"plugin"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/smoke-plugin-open.go <path-to-revenium-bifrost.so>")
		os.Exit(1)
	}
	soPath := os.Args[1]

	p, err := plugin.Open(soPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin.Open failed: %v\n", err)
		os.Exit(1)
	}

	// Soloader-required symbol list, verified verbatim from
	// github.com/maximhq/bifrost/framework/plugins/soloader.go on 2026-05-20.
	symbolNames := []string{
		"Init",
		"GetName",
		"Cleanup",
		"PreLLMHook",
		"PostLLMHook",
		"HTTPTransportStreamChunkHook",
	}

	// Stage 1: ALL six symbols must be present via plugin.Lookup. Missing any
	// of these means Bifrost's soloader would fail at load time with a
	// "symbol X not found" error — Pitfall 5 (stripped symbols) or
	// T-04-04 (signature drift to a var/method-bound form that breaks the
	// typed cast even if the name lookups).
	syms := make(map[string]plugin.Symbol, len(symbolNames))
	for _, name := range symbolNames {
		sym, err := p.Lookup(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "symbol %s missing: %v\n", name, err)
			os.Exit(2)
		}
		syms[name] = sym
	}

	// Stage 2: typed cast for the three Bifrost-typed-agnostic symbols, mirroring
	// the soloader.go-style assertion. Each failure here is a CONTRACT violation
	// that would surface as a silent plugin.Open success followed by hook
	// dispatch panic at first request — much worse than failing loudly here.
	initFn, ok := syms["Init"].(func(any) error)
	if !ok {
		fmt.Fprintf(os.Stderr, "Init: type cast to `func(any) error` failed (got %T)\n", syms["Init"])
		os.Exit(3)
	}
	_ = initFn // intentionally NOT invoked — see header comment

	nameFn, ok := syms["GetName"].(func() string)
	if !ok {
		fmt.Fprintf(os.Stderr, "GetName: type cast to `func() string` failed (got %T)\n", syms["GetName"])
		os.Exit(3)
	}
	if got := nameFn(); got != "revenium" {
		fmt.Fprintf(os.Stderr, "GetName returned %q want %q (CONTRACT-03 violation)\n", got, "revenium")
		os.Exit(4)
	}

	cleanupFn, ok := syms["Cleanup"].(func() error)
	if !ok {
		fmt.Fprintf(os.Stderr, "Cleanup: type cast to `func() error` failed (got %T)\n", syms["Cleanup"])
		os.Exit(3)
	}
	_ = cleanupFn // intentionally NOT invoked — see header comment

	fmt.Println(`OK: 6 symbols present; GetName="revenium"; Init/Cleanup/GetName cast cleanly`)
	os.Exit(0)
}
