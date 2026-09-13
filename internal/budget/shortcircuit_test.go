package budget

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildShortCircuit_PythonParityShape asserts that BuildShortCircuit
// produces a wire body byte-identical to the Python ReveniumGuardrail 429
// detail JSON (revenium_middleware_litellm_proxy/guardrail.py L162–181).
//
// This is the structural contract LiteLLM customers migrating to Bifrost
// depend on for their existing client-side budget-exhaustion UX. The test
// unmarshals the produced Error.Message into the package-level wireBody
// struct (hoisted in shortcircuit.go) so a CI failure points at the exact
// field name / type that drifted.
func TestBuildShortCircuit_PythonParityShape(t *testing.T) {
	d := &Decision{
		Allowed: false,
		Budget: &Budget{
			Name:         "daily-spend",
			Threshold:    100,
			CurrentValue: 150,
			Remaining:    -50,
			PercentUsed:  150,
			Risk:         "EXCEEDED",
		},
		ExceededCount: 1,
	}
	sc := BuildShortCircuit(d)

	// Structural assertions (BifrostError fields locked by the Phase 1 spike
	// TestContract_429NoFallback — these are the proven-on-wire values).
	require.NotNil(t, sc)
	require.NotNil(t, sc.Error)
	require.NotNil(t, sc.Error.StatusCode)
	require.Equal(t, 429, *sc.Error.StatusCode)
	require.NotNil(t, sc.Error.AllowFallbacks)
	require.False(t, *sc.Error.AllowFallbacks)
	require.False(t, sc.Error.IsBifrostError)
	require.NotNil(t, sc.Error.Error)
	require.Nil(t, sc.Response)
	require.Nil(t, sc.Stream)

	// Unmarshal Error.Message into the hoisted wireBody type. If Task 2
	// kept the wire-body types function-local instead of hoisting them to
	// package-level, this line would fail at compile time — the locked
	// reference is the Task 2 acceptance for hoisted types.
	var body wireBody
	require.NoError(t, json.Unmarshal([]byte(sc.Error.Error.Message), &body))

	// Python-parity outer fields.
	require.Equal(t, "Budget exceeded", body.Error.Message)
	require.Equal(t, "budget_exceeded", body.Error.Type)
	require.Equal(t, "revenium", body.Error.Guardrail)
	require.Len(t, body.Error.Budgets, 1)

	// Python-parity per-budget element (camelCase JSON keys verified by the
	// non-zero numeric values surviving the unmarshal round-trip).
	require.Equal(t, "daily-spend", body.Error.Budgets[0].Name)
	require.Equal(t, "EXCEEDED", body.Error.Budgets[0].Risk)
	require.Equal(t, float64(100), body.Error.Budgets[0].Threshold)
	require.Equal(t, float64(150), body.Error.Budgets[0].CurrentValue)
	require.Equal(t, float64(-50), body.Error.Budgets[0].Remaining)
	require.Equal(t, float64(150), body.Error.Budgets[0].PercentUsed)
}

// TestBuildShortCircuit_NilBudgetDefensive locks in the Pitfall 8 hard line:
// Decision{Allowed:false, Budget:nil} (the defensive path — exceededCount>0
// but no item with risk=="EXCEEDED") ALWAYS produces a NON-nil short-circuit
// with a default-shape budget element (Risk="EXCEEDED", zero numeric fields).
//
// reqID: BUDGET-06 — typed sentinel discipline (Decision{Allowed:false} as a
// typed value never collapsed to nil).
func TestBuildShortCircuit_NilBudgetDefensive(t *testing.T) {
	d := &Decision{
		Allowed:       false,
		Budget:        nil,
		ExceededCount: 1,
	}
	sc := BuildShortCircuit(d)

	// Pitfall 8 hard line: Decision{Allowed:false} always produces non-nil.
	require.NotNil(t, sc, "[BUDGET-06] Decision{Allowed:false} MUST produce non-nil short-circuit")
	require.NotNil(t, sc.Error)
	require.NotNil(t, sc.Error.Error)

	var body wireBody
	require.NoError(t, json.Unmarshal([]byte(sc.Error.Error.Message), &body))
	require.Len(t, body.Error.Budgets, 1)
	require.Equal(t, "EXCEEDED", body.Error.Budgets[0].Risk, "[BUDGET-06] default-shape Risk must be EXCEEDED")
	require.Equal(t, float64(0), body.Error.Budgets[0].Threshold, "[BUDGET-06] default-shape Threshold must be zero")
	require.Equal(t, float64(0), body.Error.Budgets[0].CurrentValue, "[BUDGET-06] default-shape CurrentValue must be zero")
}

// TestBuildShortCircuit_NilDecision asserts the defensive nil-Decision
// contract: BuildShortCircuit(nil) MUST NOT panic.
//
// Plan-locked choice (Task 2): nil-Decision returns a NON-nil short-circuit
// with a default-shape Budget so PreLLMHook's caller always holds a usable
// wire-shape on the defensive path.
func TestBuildShortCircuit_NilDecision(t *testing.T) {
	require.NotPanics(t, func() { _ = BuildShortCircuit(nil) })

	sc := BuildShortCircuit(nil)
	require.NotNil(t, sc, "BuildShortCircuit(nil) must return non-nil per Task 2 planner choice")
	require.NotNil(t, sc.Error)
	require.NotNil(t, sc.Error.StatusCode)
	require.Equal(t, 429, *sc.Error.StatusCode)
	require.NotNil(t, sc.Error.AllowFallbacks)
	require.False(t, *sc.Error.AllowFallbacks)

	// Default-shape Budget: Risk="EXCEEDED", zero numeric fields, empty Name.
	var body wireBody
	require.NoError(t, json.Unmarshal([]byte(sc.Error.Error.Message), &body))
	require.Len(t, body.Error.Budgets, 1)
	require.Equal(t, "EXCEEDED", body.Error.Budgets[0].Risk)
	require.Equal(t, "", body.Error.Budgets[0].Name)
	require.Equal(t, float64(0), body.Error.Budgets[0].Threshold)
}
