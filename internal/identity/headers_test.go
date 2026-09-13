package identity

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"

	"github.com/revenium/revenium-bifrost-plugin/internal/ctxkeys"
)

// newTestCtx constructs a real *BifrostContext usable in unit tests. Mirrors
// the in-repo pattern from plugin_recover_test.go L213 — Bifrost's
// NewBifrostContext is the only constructor that initializes the userValues
// map, so ctx.SetValue / ctx.Value round-trip works in test code.
func newTestCtx() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
}

// TestExtractKeyHash locks IDENTITY-01 / BUDGET-01 read semantics: a value
// written via ctx.SetValue(ctxkeys.KeyHeaderKeyHash, ...) is returned by
// ExtractKeyHash; absent key returns ""; nil ctx returns ""; a non-string
// value at the key returns "" (comma-ok defense — Pitfall 7).
func TestExtractKeyHash(t *testing.T) {
	tests := []struct {
		name  string
		reqID string
		ctx   *schemas.BifrostContext
		setup func(*schemas.BifrostContext)
		want  string
	}{
		{
			name:  "nil context",
			reqID: "IDENTITY-01",
			ctx:   nil,
			want:  "",
		},
		{
			name:  "key absent",
			reqID: "IDENTITY-01",
			ctx:   newTestCtx(),
			want:  "",
		},
		{
			name:  "key present",
			reqID: "IDENTITY-01",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderKeyHash, "sk-test-12345678abcdef")
			},
			want: "sk-test-12345678abcdef",
		},
		{
			name:  "empty string at key",
			reqID: "IDENTITY-01",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderKeyHash, "")
			},
			want: "",
		},
		{
			name:  "wrong-type defense (int at string key — Pitfall 7)",
			reqID: "IDENTITY-01",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderKeyHash, 42)
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil && tt.ctx != nil {
				tt.setup(tt.ctx)
			}
			got := ExtractKeyHash(tt.ctx)
			require.Equal(t, tt.want, got, "[%s]", tt.reqID)
		})
	}
}

// TestExtractSubscriber locks IDENTITY-02 contracts: id-only path,
// email-only path with credential-from-email fallback (Python
// _resolve_credentials parity), all-empty returns nil, nil-ctx returns nil.
func TestExtractSubscriber(t *testing.T) {
	tests := []struct {
		name  string
		reqID string
		ctx   *schemas.BifrostContext
		setup func(*schemas.BifrostContext)
		want  *Subscriber
	}{
		{
			name:  "nil context",
			reqID: "IDENTITY-02",
			ctx:   nil,
			want:  nil,
		},
		{
			name:  "all empty — returns nil",
			reqID: "IDENTITY-02",
			ctx:   newTestCtx(),
			want:  nil,
		},
		{
			name:  "id-only",
			reqID: "IDENTITY-02",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderSubscriberID, "user-42")
			},
			want: &Subscriber{ID: "user-42"},
		},
		{
			name:  "email-only — credential fallback to email (Python _resolve_credentials parity)",
			reqID: "IDENTITY-02",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderSubscriberEmail, "a@b.com")
			},
			want: &Subscriber{
				Email:           "a@b.com",
				CredentialName:  "a@b.com",
				CredentialValue: "a@b.com",
			},
		},
		{
			name:  "id + email — both populated, credential fallback fires from email",
			reqID: "IDENTITY-02",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderSubscriberID, "user-42")
				c.SetValue(ctxkeys.KeyHeaderSubscriberEmail, "a@b.com")
			},
			want: &Subscriber{
				ID:              "user-42",
				Email:           "a@b.com",
				CredentialName:  "a@b.com",
				CredentialValue: "a@b.com",
			},
		},
		{
			name:  "wrong-type defense on id (Pitfall 7)",
			reqID: "IDENTITY-02",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderSubscriberID, 12345)
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil && tt.ctx != nil {
				tt.setup(tt.ctx)
			}
			got := ExtractSubscriber(tt.ctx)
			require.Equal(t, tt.want, got, "[%s]", tt.reqID)
		})
	}
}

// TestExtractOrganization locks IDENTITY-03: name+id both populated when
// both ctxkeys are set; both empty returns nil; nil-ctx returns nil. The
// name→id precedence rule belongs to the metering-payload consumer (Phase
// 3), NOT here — both fields are surfaced verbatim.
func TestExtractOrganization(t *testing.T) {
	tests := []struct {
		name  string
		reqID string
		ctx   *schemas.BifrostContext
		setup func(*schemas.BifrostContext)
		want  *Organization
	}{
		{
			name:  "nil context",
			reqID: "IDENTITY-03",
			ctx:   nil,
			want:  nil,
		},
		{
			name:  "all empty — returns nil",
			reqID: "IDENTITY-03",
			ctx:   newTestCtx(),
			want:  nil,
		},
		{
			name:  "name only",
			reqID: "IDENTITY-03",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderOrgName, "acme")
			},
			want: &Organization{Name: "acme"},
		},
		{
			name:  "id only",
			reqID: "IDENTITY-03",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderOrgID, "org-1")
			},
			want: &Organization{ID: "org-1"},
		},
		{
			name:  "name + id both populated",
			reqID: "IDENTITY-03",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderOrgName, "acme")
				c.SetValue(ctxkeys.KeyHeaderOrgID, "org-1")
			},
			want: &Organization{Name: "acme", ID: "org-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil && tt.ctx != nil {
				tt.setup(tt.ctx)
			}
			got := ExtractOrganization(tt.ctx)
			require.Equal(t, tt.want, got, "[%s]", tt.reqID)
		})
	}
}

// TestExtractProduct locks IDENTITY-04: parallel shape to Organization.
func TestExtractProduct(t *testing.T) {
	tests := []struct {
		name  string
		reqID string
		ctx   *schemas.BifrostContext
		setup func(*schemas.BifrostContext)
		want  *Product
	}{
		{
			name:  "nil context",
			reqID: "IDENTITY-04",
			ctx:   nil,
			want:  nil,
		},
		{
			name:  "all empty — returns nil",
			reqID: "IDENTITY-04",
			ctx:   newTestCtx(),
			want:  nil,
		},
		{
			name:  "name only",
			reqID: "IDENTITY-04",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderProductName, "widget")
			},
			want: &Product{Name: "widget"},
		},
		{
			name:  "id only",
			reqID: "IDENTITY-04",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderProductID, "p-1")
			},
			want: &Product{ID: "p-1"},
		},
		{
			name:  "name + id both populated",
			reqID: "IDENTITY-04",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderProductName, "widget")
				c.SetValue(ctxkeys.KeyHeaderProductID, "p-1")
			},
			want: &Product{Name: "widget", ID: "p-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil && tt.ctx != nil {
				tt.setup(tt.ctx)
			}
			got := ExtractProduct(tt.ctx)
			require.Equal(t, tt.want, got, "[%s]", tt.reqID)
		})
	}
}

// TestExtractTraceContext locks IDENTITY-05: any one of the five trace
// ctxkeys present produces a populated *TraceContext with only that field
// filled (others empty string, not nil); all five empty returns nil;
// nil-ctx returns nil; mixed combinations return the correct subset.
func TestExtractTraceContext(t *testing.T) {
	tests := []struct {
		name  string
		reqID string
		ctx   *schemas.BifrostContext
		setup func(*schemas.BifrostContext)
		want  *TraceContext
	}{
		{
			name:  "nil context",
			reqID: "IDENTITY-05",
			ctx:   nil,
			want:  nil,
		},
		{
			name:  "all empty — returns nil",
			reqID: "IDENTITY-05",
			ctx:   newTestCtx(),
			want:  nil,
		},
		{
			name:  "trace-id only",
			reqID: "IDENTITY-05",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderTraceID, "trace-abc")
			},
			want: &TraceContext{TraceID: "trace-abc"},
		},
		{
			name:  "task-type only",
			reqID: "IDENTITY-05",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderTaskType, "summarization")
			},
			want: &TraceContext{TaskType: "summarization"},
		},
		{
			name:  "agent only",
			reqID: "IDENTITY-05",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderAgent, "agent-007")
			},
			want: &TraceContext{Agent: "agent-007"},
		},
		{
			name:  "subscription-id only",
			reqID: "IDENTITY-05",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderSubscriptionID, "sub-99")
			},
			want: &TraceContext{SubscriptionID: "sub-99"},
		},
		{
			name:  "response-quality-score only",
			reqID: "IDENTITY-05",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderResponseQualityScore, "0.95")
			},
			want: &TraceContext{ResponseQualityScore: "0.95"},
		},
		{
			name:  "all five populated",
			reqID: "IDENTITY-05",
			ctx:   newTestCtx(),
			setup: func(c *schemas.BifrostContext) {
				c.SetValue(ctxkeys.KeyHeaderTraceID, "trace-abc")
				c.SetValue(ctxkeys.KeyHeaderTaskType, "summarization")
				c.SetValue(ctxkeys.KeyHeaderAgent, "agent-007")
				c.SetValue(ctxkeys.KeyHeaderSubscriptionID, "sub-99")
				c.SetValue(ctxkeys.KeyHeaderResponseQualityScore, "0.95")
			},
			want: &TraceContext{
				TraceID:              "trace-abc",
				TaskType:             "summarization",
				Agent:                "agent-007",
				SubscriptionID:       "sub-99",
				ResponseQualityScore: "0.95",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil && tt.ctx != nil {
				tt.setup(tt.ctx)
			}
			got := ExtractTraceContext(tt.ctx)
			require.Equal(t, tt.want, got, "[%s]", tt.reqID)
		})
	}
}
