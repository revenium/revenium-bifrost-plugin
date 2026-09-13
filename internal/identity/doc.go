// Package identity extracts the eleven Revenium-defined x-revenium-* headers
// from the inbound Bifrost request context and resolves them into typed
// identity records (Subscriber, Organization, Product, TraceContext) plus the
// caller-supplied keyHash. Per CLAUDE.md "Configuration surface" and PROJECT.md
// "x-revenium-* headers as the sole identity source" the plugin holds no
// virtual-key registry and no server-side lookup — every identity field
// originates from one of these headers.
//
// The eleven supported headers (case-insensitive lookup; trim surrounding
// whitespace; empty after trim treated as absent) are:
//
//   - x-revenium-key-hash         — primary keyHash for budget-check + metering
//   - x-revenium-subscriber-id    — Subscriber.ID
//   - x-revenium-subscriber-email — Subscriber.Email (also used as Credential
//     fallback when the dedicated credential headers are absent)
//   - x-revenium-organization-name — Organization.Name
//   - x-revenium-organization-id   — Organization.ID
//   - x-revenium-product-name      — Product.Name
//   - x-revenium-product-id        — Product.ID
//   - x-revenium-trace-id          — TraceContext.TraceID
//   - x-revenium-task-type         — TraceContext.TaskType
//   - x-revenium-agent             — TraceContext.Agent
//   - x-revenium-subscription-id   — TraceContext.SubscriptionID
//   - x-revenium-response-quality-score — TraceContext.ResponseQualityScore
//
// Phase 1: stub. Every Extract* function returns its zero value (empty string
// or nil pointer) without inspecting the context. Phase 2 implements
// case-insensitive lookup mirroring the
// revenium-middleware-litellm-proxy-python _extract_organization_name /
// _extract_product_name fallback chains and the credential-from-email fallback
// shape from _helpers.py.
package identity
