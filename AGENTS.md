# AGENTS.md

> Machine-readable instructions for AI agents. Human docs: [README.md](README.md)

## Project Context

**Type**: Native Go Bifrost plugin (`.so`)
**Purpose**: Add Revenium pre-call budget enforcement and post-call AI usage metering to the Bifrost LLM proxy
**Stack**: Go 1.26.2 (pinned; exact — must match Bifrost `core/go.mod`)
**Module**: `github.com/revenium/revenium-bifrost-plugin`

## Commands

```bash
# Build (native -buildmode=plugin only — never cross-compile)
make build

# Test (internal/... only; top-level package main requires -buildmode=plugin)
make test

# Lint + test + build (full CI surface)
make ci

# Build the Bifrost HTTP host harness from source at the matching tag
make bifrost-host
```

The Go toolchain is pinned via `GOTOOLCHAIN=go1.26.2` in the `Makefile`. Do not invoke `go build`, `go test`, etc. directly without re-applying the toolchain pin — patch-version drift breaks `plugin.Open` at load time.

## Architecture

```
revenium-bifrost-plugin/
├── plugin.go                         # Top-level package main — declares the 7 Bifrost-required exports
├── internal/
│   ├── config/                       # Env-var parsing (REVENIUM_METERING_*, REVENIUM_DEBUG, REVENIUM_DRY_RUN)
│   ├── ctxkeys/                      # Bifrost context keys (Pitfall 17 namespace: revenium-bifrost.*)
│   ├── plog/                         # Plugin logging — bridges to schemas.Logger
│   ├── identity/                     # x-revenium-* header extractors
│   ├── budget/                       # Budget-check HTTP client (3.0s timeout, fail-open)
│   ├── metering/                     # PayloadBuilder + MeteringClient wiring (revenium-go-sdk)
│   └── streaming/                    # Chunk accumulator + TTFT capture for streamed responses
└── test/integration/                 # Separate Go module — integration tests against fake Bifrost
```

## Critical Constraints

1. **Go toolchain pin (1.26.2 exact)** — Any patch-version drift between plugin and Bifrost host causes `plugin.Open` to fail with `package was built with a different version of package runtime`. Pin in `go.mod`, `.go-version`, `GOTOOLCHAIN`, and CI matrix.
2. **Pitfall 5 — no symbol-stripping ldflags** — Do **not** pass `-ldflags=-s` or `-ldflags=-w` to the build. Both strip the symbol table that `plugin.Lookup("Init")` requires. The plugin will load and immediately fail every symbol-lookup.
3. **Pitfall 17 — ctxkey namespace** — All `schemas.BifrostContextKey` values used by the plugin must be prefixed with `revenium-bifrost.`. Bifrost's `BifrostContext.SetValue/Value` uses a flat keyspace shared across every loaded plugin; unprefixed names like `trace-id` will silently clobber other plugins' keys.
4. **`middleware_source="GUARDRAIL"`** — Metering payloads emitted by this plugin set the `middleware_source` field to the literal string `GUARDRAIL`. This is the analytics-side distinguisher between this plugin's payloads and any other Revenium metering source.
5. **Fail-open on budget-check failures** — Transport errors, non-200 responses, and timeouts on the budget-check endpoint must allow the request through (the LiteLLM `ReveniumGuardrail` baseline). Only an explicit `block` decision short-circuits.
6. **Native `plugin` package only** — Cross-compilation of `-buildmode=plugin` is structurally unsupported. Each target OS/arch must build on a matching native runner. `BUILD-06` documents this in detail.

## Environment Variables

```bash
REVENIUM_METERING_BASE_URL=https://api.revenium.ai  # Required: Revenium API root
REVENIUM_METERING_API_KEY=<secret>                  # Required: Revenium metering API key (sourced from secret store)
REVENIUM_DEBUG=true                                 # Optional: elevate log level from INFO to DEBUG
REVENIUM_DRY_RUN=true                               # Optional: run budget-check but ALWAYS allow the request
```

If either required variable is missing, the plugin aborts at load time. Truthy values for the optional variables are `true`, `1`, `yes`, `on` (case-insensitive); anything else is false.

## Metering Endpoints

| Type         | Endpoint                                      | Key Fields                                        |
| ------------ | --------------------------------------------- | ------------------------------------------------- |
| Budget check | `GET {base}/v2/api/virtual-keys/budget-check` | `key-hash` (driven by `x-revenium-key-hash`)      |
| Metering     | `POST {base}/meter/v2/ai/completions`         | `subscriber`, `tokens`, `model`, `traceId`, etc.  |

The Revenium Go SDK (`github.com/revenium/revenium-go-sdk`) handles both clients with shared `*http.Client` pooling, retry, and circuit-breaker logic. Do not wrap `MeteringClient.Send()` in your own goroutine pool — the SDK already does `wg.Add(1); go func() { ...SendSync()... }()` internally.

## Common Errors

| Error                                                              | Fix                                                                          |
| ------------------------------------------------------------------ | ---------------------------------------------------------------------------- |
| `plugin.Open fails: package runtime was built with a different version` | Go toolchain mismatch between plugin and Bifrost host. Run `go version -m /path/to/bifrost-http \| head -1` to read the host's Go version; rebuild the plugin against it. |
| `nm symbol count != 7`                                             | Build-flag drift (most often accidental `-ldflags=-s` or `-ldflags=-w`). Rebuild without strip flags and re-verify with the `nm` one-liner in README "Source-Build Path". |
| `metering not tracking`                                            | Check `REVENIUM_METERING_API_KEY` is set on the Bifrost process environment (not just your shell). Confirm with `REVENIUM_DEBUG=true` and watching for `[Revenium]` log lines on each request. |

## References

- [README.md](README.md) — full public-facing documentation
- [CONTRIBUTING.md](CONTRIBUTING.md) — contributor workflow + release procedure
- [GitHub Repository](https://github.com/revenium/revenium-bifrost-plugin)
- [Revenium Docs](https://docs.revenium.io)
- [Bifrost Docs](https://docs.getbifrost.ai/)
- [AGENTS.md Spec](https://agents.md/)
