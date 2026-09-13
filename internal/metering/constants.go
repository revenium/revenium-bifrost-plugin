// Package metering wraps the revenium-go-sdk/core/metering MeteringClient and
// declares the Phase-3 PayloadBuilder shells (BuildSuccess, BuildFailure,
// BuildBlocked, BuildStream). Phase 1 ships:
//
//   - Real revsdk.MeteringClient construction in New() (D-12) so Plan 04's
//     Init can wire a fully-functional client into package-level state.
//   - Send / Close pass-throughs to the SDK (NOT goroutine-wrapped per
//     CLAUDE.md "Don't Hand-Roll" rule 1 — the SDK already manages its own
//     goroutine pool + WaitGroup + circuit breaker + retry; wrapping it would
//     create lost-on-shutdown bugs).
//   - MiddlewareSourceGuardrail = "GUARDRAIL" constant per D-13.
//   - Four Build* stub functions with the parameter lists Phase 3 needs;
//     bodies return nil. BuildBlocked imports internal/budget (the one
//     permitted cross-package direction per Warning 8).
package metering

// MiddlewareSourceGuardrail is the value Phase 3 hook bodies pass as the
// middleware_source field on every metering payload, per D-13 (and matching
// the LiteLLM ReveniumGuardrail "GUARDRAIL" string).
//
// Phase 1 hard-codes this per D-13; the open question of Bifrost-vs-LiteLLM
// differentiation (a hypothetical "BIFROST_GUARDRAIL" enum value) is logged
// in 01-SPIKE-FINDINGS.md and tracked as a one-line change once the Revenium
// platform team coordinates a new enum value, if any.
const MiddlewareSourceGuardrail = "GUARDRAIL"
