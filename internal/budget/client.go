// PACKAGE RULE: internal/budget MUST NOT import internal/metering. See
// 01-03-PLAN.md Warning 8. The emit-on-block flow (PreLLMHook short-circuit
// -> PostLLMHook emit metering.BuildBlocked) is orchestrated by plugin.go's
// hook bodies, NOT by code in this package. Adding an internal/metering
// import here would create a circular dependency the day
// metering.BuildBlocked references *budget.Decision (which the Phase 1 stub
// signature already does). The grep gate in Plan 03 Task 5 enforces this.

package budget

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/revenium/revenium-bifrost-plugin/internal/config"
	"github.com/revenium/revenium-bifrost-plugin/internal/plog"
)

// Client owns a single shared *http.Client used to call the Revenium
// budget-check endpoint. The HTTP client is constructed exactly once in New;
// every PreLLMHook call reuses it (single-shared-client requirement per
// CLAUDE.md "Performance" + Pattern 1 in ARCHITECTURE.md).
type Client struct {
	http   *http.Client
	cfg    *config.Config
	logger plog.Logger
}

// New builds the *http.Client with the production-ready knobs needed for the
// Phase 2 budget-check path (3.0s overall timeout matching the Python
// guardrail, MaxIdleConnsPerHost=100 for connection-pool reuse across hook
// invocations, ResponseHeaderTimeout=2.5s as a sub-budget so a slow Revenium
// response surfaces as an ErrTransport rather than consuming the full 3s
// budget on connection-establishment). REAL construction per D-12 — no env
// reads inside this function, callers pass the immutable *config.Config.
func New(cfg *config.Config, logger plog.Logger) *Client {
	return &Client{
		http: &http.Client{
			Timeout: cfg.BudgetCheckTimeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   100,
				ResponseHeaderTimeout: 2500 * time.Millisecond,
			},
		},
		cfg:    cfg,
		logger: logger,
	}
}

// Check performs the GET /v2/api/virtual-keys/budget-check call against the
// configured Revenium base URL and returns a typed *Decision.
//
// Contract (Phase 2 — BUDGET-02..06):
//   - Empty keyHash → (Decision{Allowed:true}, nil) without issuing any HTTP
//     request. Defense-in-depth for BUDGET-01 callers that didn't pre-filter.
//   - HTTP 200 + summary.exceededCount == 0 → (Decision{Allowed:true,
//     ExceededCount:0}, nil). BUDGET-02 happy path.
//   - HTTP 200 + summary.exceededCount > 0 → (Decision{Allowed:false,
//     Budget:&Budget{...}, ExceededCount:N}, nil). BUDGET-03 block path.
//   - Transport error (network failure, dial timeout, DNS, malformed URL,
//     timeout) → (nil, *ErrTransport{Inner: underlying-err}). BUDGET-04.
//   - Non-200 responses (5xx, other 4xx) → (nil, *ErrTransport{Inner:
//     fmt.Errorf("budget-check returned HTTP N")}). BUDGET-05.
//   - Malformed JSON in a 200 response → (nil, *ErrTransport{Inner: wrapped
//     decode error}). BUDGET-05.
//
// Pitfall 8 invariant: Decision{Allowed:false} is a typed VALUE returned via
// the *Decision slot — NEVER wrapped in an error. The error slot is reserved
// exclusively for *ErrTransport. PreLLMHook's caller dispatches on the
// typed sentinel via errors.As(err, &transportErr).
//
// Pitfall 10 invariant: every successful *http.Client.Do is followed by
// defer resp.Body.Close() — bodyclose lint enforces.
func (c *Client) Check(ctx *schemas.BifrostContext, keyHash string) (*Decision, error) {
	// Defense in depth (BUDGET-01 fallback): if a caller forgets to pre-filter
	// the empty-keyHash case, return an allowed decision without issuing any
	// HTTP request. PreLLMHook in Plan 02-04 will short-circuit before reaching
	// here, but this guard means budget.Check is safe to call unconditionally.
	if keyHash == "" {
		return &Decision{Allowed: true}, nil
	}

	// Build URL via net/url so non-ASCII / reserved characters in the keyHash
	// are properly percent-encoded. Future-proofs against any change in Revenium
	// key hash format that introduces non-URL-safe characters.
	u := c.cfg.BaseURL + "/v2/api/virtual-keys/budget-check"
	q := url.Values{}
	q.Set("keyHash", keyHash)

	// Pick the request context: BifrostContext satisfies context.Context (per
	// core@v1.5.11/schemas/context.go — verified during Phase 1 spike). The
	// background fallback prevents a nil-deref on the rare programmatic-entry
	// path; http.NewRequestWithContext rejects a nil ctx.
	reqCtx := context.Background()
	if ctx != nil {
		reqCtx = ctx
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u+"?"+q.Encode(), nil)
	if err != nil {
		return nil, &ErrTransport{Inner: err}
	}
	httpReq.Header.Set("x-api-key", c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &ErrTransport{Inner: err}
	}
	// Pitfall 10 / bodyclose lint — close the body on every success branch.
	defer func() { _ = resp.Body.Close() }()

	// Non-200 fails open via *ErrTransport. Covers BUDGET-05 (all 5xx + all
	// non-block 4xx). The Revenium API does not signal budget-exceeded via a
	// non-200 status code — that path is HTTP 200 with summary.exceededCount > 0.
	if resp.StatusCode != http.StatusOK {
		return nil, &ErrTransport{Inner: fmt.Errorf("budget-check returned HTTP %d", resp.StatusCode)}
	}

	// Streaming JSON decode (per RESEARCH §"Don't Hand-Roll" — preferred over
	// io.ReadAll + json.Unmarshal). The anonymous struct mirrors the Revenium
	// API response shape verbatim.
	var body struct {
		Summary struct {
			ExceededCount int `json:"exceededCount"`
		} `json:"summary"`
		Items []struct {
			Name         string  `json:"name"`
			Threshold    float64 `json:"threshold"`
			CurrentValue float64 `json:"currentValue"`
			Remaining    float64 `json:"remaining"`
			PercentUsed  float64 `json:"percentUsed"`
			Risk         string  `json:"risk"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, &ErrTransport{Inner: fmt.Errorf("budget-check JSON decode: %w", err)}
	}

	// Allowed branch (BUDGET-02): exceededCount <= 0 means the subscriber is
	// under budget. Surface the exceeded count verbatim so downstream metering
	// can record it as a "would-have-blocked-near-threshold" signal in Phase 3.
	if body.Summary.ExceededCount <= 0 {
		return &Decision{Allowed: true, ExceededCount: body.Summary.ExceededCount}, nil
	}

	// Exceeded branch (BUDGET-03): walk Items looking for the first risk ==
	// "EXCEEDED" entry. Map it into *Budget. If no item has risk=="EXCEEDED"
	// (defensive — should not happen when exceededCount > 0), return
	// Allowed=false with a nil Budget. The consumer (BuildShortCircuit in
	// shortcircuit.go) passes nil-Budget to a default-shape detail body.
	var exceeded *Budget
	for _, it := range body.Items {
		if it.Risk == "EXCEEDED" {
			exceeded = &Budget{
				Name:         it.Name,
				Threshold:    it.Threshold,
				CurrentValue: it.CurrentValue,
				Remaining:    it.Remaining,
				PercentUsed:  it.PercentUsed,
				Risk:         it.Risk,
			}
			break
		}
	}
	return &Decision{
		Allowed:       false,
		Budget:        exceeded,
		ExceededCount: body.Summary.ExceededCount,
	}, nil
}

// Close closes idle HTTP connections held by the pooled transport. REAL
// implementation: Cleanup() in plugin.go calls this so the budget client's
// connection pool releases cleanly on shutdown. Safe to call multiple times;
// the underlying http.Client.CloseIdleConnections is a no-op on second call.
func (c *Client) Close() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}
