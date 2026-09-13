package budget

import (
	"encoding/json"

	"github.com/maximhq/bifrost/core/schemas"
)

// --- Wire-body shape (package-level, unexported) ---
//
// These three types lock the Python-parity 429 detail-body shape that
// BuildShortCircuit emits. Hoisted to package-level (NOT function-local) so
// shortcircuit_test.go can `json.Unmarshal` into the typed structs instead of
// `map[string]any` — see Plan 02-03 Task 2 for the hoisting decision and
// Task 4 for the consumer test functions.
//
// JSON tags use camelCase (matching the Python ReveniumGuardrail wire format
// at revenium_middleware_litellm_proxy/guardrail.py L162–181). This is the
// byte-identical wire contract LiteLLM customers migrating to Bifrost
// depend on for their client-side budget-exhaustion UX.

// budgetDetail is the per-budget element in the 429 detail body. Each field
// maps directly to a *Budget field on the *Decision passed to BuildShortCircuit.
type budgetDetail struct {
	Name         string  `json:"name"`
	Threshold    float64 `json:"threshold"`
	CurrentValue float64 `json:"currentValue"`
	Remaining    float64 `json:"remaining"`
	PercentUsed  float64 `json:"percentUsed"`
	Risk         string  `json:"risk"`
}

// errorDetail is the inner `error` object of the 429 detail body. Fields
// match the Python guardrail's `error` dict shape:
//
//	{"message": "Budget exceeded", "type": "budget_exceeded",
//	 "guardrail": "revenium", "budgets": [{...}]}
type errorDetail struct {
	Message   string         `json:"message"`
	Type      string         `json:"type"`
	Guardrail string         `json:"guardrail"`
	Budgets   []budgetDetail `json:"budgets"`
}

// wireBody is the top-level envelope of the 429 detail body. The outer
// `{"error":{...}}` shape matches OpenAI's standard error envelope so LLM
// clients can parse the response with their existing error-handling code.
type wireBody struct {
	Error errorDetail `json:"error"`
}

// BuildShortCircuit constructs the *schemas.LLMPluginShortCircuit returned by
// PreLLMHook when a Decision blocks the request. Phase 1 stub returned nil;
// Phase 2 implements the body with:
//
//   - BifrostError.StatusCode=&429 + AllowFallbacks=&false (per the Phase 1
//     spike's TestContract_429NoFallback verification — proven on the wire as
//     a real HTTP 429 with no provider fallback).
//   - IsBifrostError=false (signals "this came from a plugin, not Bifrost core").
//   - Error.Message: a JSON-encoded body whose top-level shape is
//     `{"error":{"message":"Budget exceeded","type":"budget_exceeded",
//     "guardrail":"revenium","budgets":[{...}]}}` — byte-identical to the
//     Python ReveniumGuardrail wire format at
//     revenium_middleware_litellm_proxy/guardrail.py L162–181.
//
// Pitfall 8 hard line: BuildShortCircuit(&Decision{Allowed:false, Budget:nil})
// returns a NON-nil short-circuit with a default-shape budget element
// (Risk="EXCEEDED", zero numeric fields). This defends against the defensive
// path where exceededCount>0 but no item with risk=="EXCEEDED" was found in
// the budget-check response.
//
// Defensive nil-Decision: BuildShortCircuit(nil) does NOT panic. It returns a
// NON-nil short-circuit with the default-shape Budget — keeps PreLLMHook's
// caller always holding a usable wire-shape even on the rare nil-Decision
// path.
//
// The wire-body types (wireBody/errorDetail/budgetDetail) are hoisted to
// package-level so shortcircuit_test.go (Task 4) can json.Unmarshal the
// produced Error.Message into the typed struct.
//
// See 01-RESEARCH.md "Code Examples > Verified LLMPluginShortCircuit shape"
// for the spike-proven structural shape and 02-RESEARCH.md "Example C" for
// the Python-parity detail-body shape.
func BuildShortCircuit(d *Decision) *schemas.LLMPluginShortCircuit {
	// Local pointer-to-int / pointer-to-bool — &code and &falseVal produce
	// stable pointers for the BifrostError fields. Spike-proven shape.
	code := 429
	falseVal := false

	// Build the per-budget detail. When d.Budget is populated (the common
	// path — populated by Check from the first risk=="EXCEEDED" item),
	// copy all six fields verbatim. When d is nil OR d.Budget is nil
	// (defensive path — exceededCount>0 but no EXCEEDED item, OR caller
	// passed nil), emit a default-shape detail with Risk="EXCEEDED" so the
	// wire body still carries a usable contract signal.
	detail := budgetDetail{Risk: "EXCEEDED"}
	if d != nil && d.Budget != nil {
		detail = budgetDetail{
			Name:         d.Budget.Name,
			Threshold:    d.Budget.Threshold,
			CurrentValue: d.Budget.CurrentValue,
			Remaining:    d.Budget.Remaining,
			PercentUsed:  d.Budget.PercentUsed,
			Risk:         d.Budget.Risk,
		}
	}

	body := wireBody{
		Error: errorDetail{
			Message:   "Budget exceeded",
			Type:      "budget_exceeded",
			Guardrail: "revenium",
			Budgets:   []budgetDetail{detail},
		},
	}

	// json.Marshal of strings+floats cannot fail — error discarded intentionally
	// (the only way json.Marshal returns an error for the above struct shape
	// would be a programmer-introduced unsupported type, which would surface
	// in shortcircuit_test.go at first build).
	raw, _ := json.Marshal(body)

	return &schemas.LLMPluginShortCircuit{
		Error: &schemas.BifrostError{
			IsBifrostError: false,
			StatusCode:     &code,
			Error: &schemas.ErrorField{
				Message: string(raw),
			},
			AllowFallbacks: &falseVal,
		},
	}
}
