// Package budget owns the live budget-check call to Revenium's
// GET /v2/api/virtual-keys/budget-check endpoint and the construction of the
// short-circuit response when the subscriber is over budget. Phase 1 ships the
// types + signatures + a real *http.Client (constructed in New per D-12) so
// Plan 04's Init can declare a package-level *budget.Client and Plan 04+ can
// import the type. Phase 2 implements the actual Check() body, the
// fail-open transport-error classification, and the BuildShortCircuit body
// with StatusCode=&429 + AllowFallbacks=&false.
package budget

import (
	"fmt"
)

// Decision is the outcome of a budget-check call. Allowed=false means the
// caller's request must be blocked; Budget describes which threshold tripped.
// ExceededCount mirrors the budget-check response field (>0 means "blocked").
type Decision struct {
	Allowed       bool
	Budget        *Budget
	ExceededCount int
}

// Budget captures the matched budget definition returned by the budget-check
// endpoint. All fields are populated from the JSON response body in Phase 2.
type Budget struct {
	Name         string
	Threshold    float64
	CurrentValue float64
	Remaining    float64
	PercentUsed  float64
	Risk         string
}

// ErrTransport is the typed sentinel wrapping any non-classification error
// from the budget-check call (DNS, dial timeout, TLS handshake, 5xx, malformed
// response body, etc.). Phase 2's PreLLMHook MUST `errors.As` against
// *ErrTransport to satisfy BUDGET-06 — only an explicit budget-exceeded
// Decision (Allowed=false) yields a short-circuit; any *ErrTransport fails
// open per CLAUDE.md "fail-open on budget-check transport errors".
//
// Unwrap is implemented so callers can chain errors.Is/As against any inner
// stdlib error type (e.g., *net.OpError, *url.Error, context.DeadlineExceeded).
type ErrTransport struct {
	Inner error
}

func (e *ErrTransport) Error() string {
	return fmt.Sprintf("budget transport error: %v", e.Inner)
}

func (e *ErrTransport) Unwrap() error { return e.Inner }
