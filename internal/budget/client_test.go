package budget

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/config"
	"github.com/revenium/revenium-bifrost-plugin/internal/plog"
)

// TestClient_Check is the table-driven coverage for (*Client).Check covering
// BUDGET-02..06 via reqID-tagged rows (D-22 traceability per CONTEXT.md).
//
// D-23: every row exercises the PRODUCTION *Client constructor via
// base-URL rewrite ONLY — no BudgetChecker interface, no test seam. The
// production *http.Client IS the test *http.Client; httptest.NewServer
// gives us a real on-wire round trip including pooled-connection + 2.5s
// ResponseHeaderTimeout sub-budget behavior.
//
// Pitfall 10 evidence: row "timeout (BUDGET-04 / Pitfall 10)" asserts the
// 2.5s ResponseHeaderTimeout sub-budget trips BEFORE the 3.0s outer
// http.Client.Timeout cliff (wall-time bound of 3*time.Second).
func TestClient_Check(t *testing.T) {
	tests := []struct {
		name                string
		reqID               string // BUDGET-NN traceability per D-22
		srvHandler          http.HandlerFunc
		srvClosedBeforeCall bool
		keyHash             string
		wantDec             *Decision
		wantErrIs           func(error) bool
		maxWallTime         time.Duration
	}{
		{
			name:  "allowed (BUDGET-02 happy path)",
			reqID: "BUDGET-02",
			srvHandler: func(w http.ResponseWriter, r *http.Request) {
				// Row 10 request-shape verification (Behavior 8): assert GET,
				// path, query, x-api-key header. This is the canonical
				// request-shape lock-in row.
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/v2/api/virtual-keys/budget-check", r.URL.Path)
				require.Equal(t, "sk-test-allowed", r.URL.Query().Get("keyHash"))
				require.Equal(t, "test-api-key", r.Header.Get("x-api-key"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"summary":{"exceededCount":0},"items":[]}`))
			},
			keyHash:   "sk-test-allowed",
			wantDec:   &Decision{Allowed: true, ExceededCount: 0},
			wantErrIs: nil,
		},
		{
			name:  "exceeded (BUDGET-03)",
			reqID: "BUDGET-03",
			srvHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"summary":{"exceededCount":1},"items":[{"name":"daily","threshold":100,"currentValue":150,"remaining":-50,"percentUsed":150,"risk":"EXCEEDED"}]}`))
			},
			keyHash: "sk-test-exceeded",
			wantDec: &Decision{
				Allowed: false,
				Budget: &Budget{
					Name:         "daily",
					Threshold:    100,
					CurrentValue: 150,
					Remaining:    -50,
					PercentUsed:  150,
					Risk:         "EXCEEDED",
				},
				ExceededCount: 1,
			},
			wantErrIs: nil,
		},
		{
			name:                "transport error - connection refused (BUDGET-04)",
			reqID:               "BUDGET-04",
			srvHandler:          func(w http.ResponseWriter, r *http.Request) {},
			srvClosedBeforeCall: true,
			keyHash:             "sk-test-conn-refused",
			wantDec:             nil,
			wantErrIs: func(err error) bool {
				var t *ErrTransport
				return errors.As(err, &t)
			},
		},
		{
			name:  "non-200 HTTP 500 (BUDGET-05)",
			reqID: "BUDGET-05",
			srvHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			keyHash: "sk-test-500",
			wantDec: nil,
			wantErrIs: func(err error) bool {
				var t *ErrTransport
				if !errors.As(err, &t) {
					return false
				}
				// BUDGET-05 contract: the wrapped Inner error must mention
				// "HTTP 500" so operators see the upstream status in logs.
				return t.Inner != nil && containsErr(t.Inner, "HTTP 500")
			},
		},
		{
			name:  "non-200 HTTP 401 - 4xx other (BUDGET-05)",
			reqID: "BUDGET-05",
			srvHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			keyHash: "sk-test-401",
			wantDec: nil,
			wantErrIs: func(err error) bool {
				var t *ErrTransport
				if !errors.As(err, &t) {
					return false
				}
				return t.Inner != nil && containsErr(t.Inner, "HTTP 401")
			},
		},
		{
			name:  "malformed JSON in 200 response (BUDGET-05)",
			reqID: "BUDGET-05",
			srvHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{not valid json`))
			},
			keyHash: "sk-test-malformed",
			wantDec: nil,
			wantErrIs: func(err error) bool {
				var t *ErrTransport
				if !errors.As(err, &t) {
					return false
				}
				// The decoder error message contains "decode" via fmt.Errorf wrap.
				return t.Inner != nil && containsErr(t.Inner, "decode")
			},
		},
		{
			name:  "timeout via ResponseHeaderTimeout sub-budget (BUDGET-04 / Pitfall 10)",
			reqID: "BUDGET-04",
			srvHandler: func(w http.ResponseWriter, r *http.Request) {
				// Sleep 3s before responding. The 2.5s ResponseHeaderTimeout
				// sub-budget MUST trip first, returning *ErrTransport BEFORE
				// the 3.0s outer http.Client.Timeout cliff.
				time.Sleep(3 * time.Second)
			},
			keyHash: "sk-test-timeout",
			wantDec: nil,
			wantErrIs: func(err error) bool {
				var t *ErrTransport
				return errors.As(err, &t)
			},
			maxWallTime: 3 * time.Second,
		},
		{
			name:    "empty keyHash (BUDGET-06 defense in depth)",
			reqID:   "BUDGET-06",
			keyHash: "",
			// srvHandler intentionally nil — no HTTP request is issued for
			// this row, so no server is spun up. The handler-counter check
			// is implicit: any server-side write would panic on a nil handler.
			wantDec:   &Decision{Allowed: true},
			wantErrIs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Per row: spin up server when handler is non-nil. The empty-keyHash
			// row (BUDGET-06 defense) deliberately leaves srvHandler nil and
			// constructs a Client against a dummy BaseURL; no HTTP request is
			// issued so the dummy URL is never dialed.
			var srvURL string
			if tt.srvHandler != nil {
				srv := httptest.NewServer(tt.srvHandler)
				srvURL = srv.URL
				if tt.srvClosedBeforeCall {
					// Close BEFORE the test calls Check — connection refused.
					srv.Close()
				} else {
					defer srv.Close()
				}
			} else {
				// Empty-keyHash row: use a deliberately-unreachable URL. The
				// Check body's early-return ensures we never dial.
				srvURL = "http://127.0.0.1:1"
			}

			cfg := &config.Config{
				BaseURL:            srvURL,
				APIKey:             "test-api-key",
				BudgetCheckTimeout: 3 * time.Second,
			}
			c := New(cfg, plog.New(false))
			defer c.Close()

			start := time.Now()
			got, err := c.Check(nil, tt.keyHash)
			elapsed := time.Since(start)

			if tt.wantErrIs != nil {
				require.Error(t, err, "[%s] expected error", tt.reqID)
				require.True(t, tt.wantErrIs(err), "[%s] err type/content assertion failed: %v", tt.reqID, err)
				require.Nil(t, got, "[%s] decision must be nil on error", tt.reqID)
			} else {
				require.NoError(t, err, "[%s]", tt.reqID)
				require.Equal(t, tt.wantDec, got, "[%s] decision diff", tt.reqID)
			}

			if tt.maxWallTime > 0 {
				require.Less(t, elapsed, tt.maxWallTime, "[%s] elapsed=%v must be < %v (ResponseHeaderTimeout sub-budget per Pitfall 10)", tt.reqID, elapsed, tt.maxWallTime)
			}
		})
	}
}

// containsErr returns true when err.Error() contains substr. Lightweight
// inline helper so the table-driven test rows can assert on wrapped error
// message content without pulling in a regex dep.
func containsErr(err error, substr string) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
