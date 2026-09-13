# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-05-25

Initial public release of the Revenium Bifrost Plugin. This is the v1 surface that ports the LiteLLM `ReveniumGuardrail`'s pre-call budget enforcement and post-call usage metering into Bifrost's native plugin model. Operators running Bifrost as their LLM gateway can now achieve the same unified spend control and AI usage analytics as a LiteLLM-only deployment.

### Added

- **PreLLMHook budget enforcement.** The plugin reads `x-revenium-key-hash` from the request context, calls the Revenium `GET /v2/api/virtual-keys/budget-check` endpoint, and short-circuits the request with HTTP 429 and `AllowFallbacks=false` on a `block` decision. The pre-call path **fails open** on transport errors, non-200 budget-check responses, and timeouts (3.0s budget) — production availability is prioritized over enforcement, matching the LiteLLM behavioral baseline. The explicit budget-exceeded short-circuit is re-raised cleanly and never swallowed.
- **PostLLMHook metering with success/failure/blocked branches.** After the upstream provider returns, the plugin emits a metering event to the Revenium meter ingest endpoint. The `KeyShortCircuited` context-key symmetry guard ensures a budget-blocked request is metered exactly once (as a "blocked" event), preventing the double-counting failure mode where Bifrost's `PostLLMHook` symmetry guarantee would otherwise fire even after a `PreLLMHook` short-circuit. Failure events are emitted with the upstream error reason attached.
- **HTTPTransportStreamChunkHook streaming roll-up.** Streaming chat completions are accumulated across chunks. The plugin captures **time-to-first-token (TTFT)** on the first non-terminal chunk, tracks chunk count and per-chunk usage where available, and emits a single rolled-up metering event on the terminal chunk. For streaming requests, `PostLLMHook` is a no-op — only the chunk hook emits, which avoids double-counting and ensures the metering payload reflects the full streamed session.
- **12 `x-revenium-*` identity headers.** Every LLM call surfaces the following caller-supplied headers into the metering payload (all optional, all case-insensitive): `x-revenium-key-hash` (drives budget check), `x-revenium-subscriber-id` and `x-revenium-subscriber-email`, `x-revenium-organization-name` and `x-revenium-organization-id` (name → id precedence), `x-revenium-product-name` and `x-revenium-product-id` (name → id precedence), `x-revenium-trace-id`, `x-revenium-task-type`, `x-revenium-agent`, `x-revenium-subscription-id`, and `x-revenium-response-quality-score`. Wiring runs through `HTTPTransportPreHook` + the `internal/identity` extractor helpers.
- **Native `.so` artifacts for linux/amd64, linux/arm64, darwin/amd64, and darwin/arm64.** Each release tag publishes four pre-built shared objects matching the filename pattern `revenium-bifrost_<TAG>_<GOOS>_<GOARCH>_go1.26.2.so`, plus per-artifact `.sha256` files and a combined `checksums.txt`. Builds run natively on four GitHub-hosted runners (`ubuntu-24.04`, `ubuntu-24.04-arm`, `macos-15-intel`, `macos-14`) per BUILD-02; cross-compilation of `-buildmode=plugin` is structurally unsupported by the Go toolchain and is not attempted.
- **Signed macOS artifacts.** The macOS `.so` files are ad-hoc codesigned (`codesign --sign -`) at release time so they load on a default-policy macOS host without intervention. Quarantine-enforced hosts can strip the quarantine flag with `xattr -d com.apple.quarantine` after verifying the published `.sha256`. Apple Developer ID signing and notarization are tracked as a future enhancement.
- **Bifrost v1.5.x compatibility.** The plugin is validated against `core/v1.5.11` + `transports/v1.5.3` on Go 1.26.2 (the exact toolchain pin Bifrost's `core/go.mod` declares; matching it is a hard prerequisite for `plugin.Open` to succeed). The compatibility matrix in the README documents the single supported tuple for this release.
- **Behavioral parity with LiteLLM ReveniumGuardrail.** Same budget-check endpoint, same fail-open posture, same metering payload shape, same identity-header surface. Subscribers and budget configuration carry over unchanged between deployments.

### Configuration surface

- Environment variables only: `REVENIUM_METERING_BASE_URL` and `REVENIUM_METERING_API_KEY` are required; `REVENIUM_DEBUG` (truthy elevates log level) and `REVENIUM_DRY_RUN` (truthy runs budget-check without short-circuiting and attaches a `would_have_blocked` flag to the metering payload) are optional.
- No plugin-specific `config.json` block in v1.
- Caller-side per-request configuration via the 12 `x-revenium-*` headers above.

### Operational notes

- The plugin is one-shot per process: there is no hot-reload path (Go `plugin` package contract). Restart Bifrost to reconfigure.
- Kubernetes deployments should set `terminationGracePeriodSeconds: 30` to allow the synchronous metering-buffer flush during `Cleanup` to drain. The Phase-1 spike measured the SIGTERM-to-Cleanup delta at 2.115383s; 30s is a comfortable safety margin.
- Windows is not supported. Bifrost's native plugin loader is Linux + macOS only.

[Unreleased]: https://github.com/revenium/revenium-bifrost-plugin/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/revenium/revenium-bifrost-plugin/releases/tag/v0.1.0
