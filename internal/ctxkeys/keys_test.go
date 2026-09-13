package ctxkeys

import (
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

// allKeys is the canonical list of every BifrostContextKey the plugin
// exports. Tests below iterate this slice; if a new key is added it MUST be
// appended here AND included in the TestKeyValues expected-value map.
func allKeys() []schemas.BifrostContextKey {
	return []schemas.BifrostContextKey{
		// Phase 1 (original five).
		KeyHash,
		KeyRequestStart,
		KeyStreamState,
		KeyDryRunDecision,
		KeyShortCircuited,
		// Phase 3 Plan 03-02 (Delta D) — RequestType pre-stash so PostLLMHook
		// can detect streaming without access to *BifrostRequest.
		KeyRequestType,
		// quick-260617-vhq — requested model/provider stashed by PreLLMHook so
		// the BUDGET_EXCEEDED block event carries a non-blank model.
		KeyRequestModel,
		KeyRequestProvider,
		// Phase 2 D-19 — twelve KeyHeader* constants, one per
		// x-revenium-* identity header. Written by HTTPTransportPreHook
		// (Plan 02-02); read by internal/identity.Extract* (Plan 02-01).
		KeyHeaderKeyHash,
		KeyHeaderSubscriberID,
		KeyHeaderSubscriberEmail,
		KeyHeaderOrgName,
		KeyHeaderOrgID,
		KeyHeaderProductName,
		KeyHeaderProductID,
		KeyHeaderTraceID,
		KeyHeaderTaskType,
		KeyHeaderAgent,
		KeyHeaderSubscriptionID,
		KeyHeaderResponseQualityScore,
	}
}

// TestKeysHavePrefix asserts every exported context-key value starts with the
// reserved "revenium-bifrost." namespace prefix per Pitfall 17 (multi-plugin
// keyspace collisions). Other Bifrost plugins use bare names like "trace-id"
// or "bf-governance-*"; this prefix is our compile-time-checked contract that
// none of our keys can collide with anyone else's.
func TestKeysHavePrefix(t *testing.T) {
	const want = "revenium-bifrost."
	for _, k := range allKeys() {
		require.Truef(
			t,
			strings.HasPrefix(string(k), want),
			"key %q must have prefix %q (Pitfall 17 namespace contract)",
			string(k),
			want,
		)
	}
}

// TestKeysAreUnique guards against accidental constant-edit collisions where
// two of our own keys end up with the same string value (e.g., a copy-paste
// that forgets to rename the right-hand side). Building a set from the slice
// and asserting set-size == slice-size is the standard idiom.
func TestKeysAreUnique(t *testing.T) {
	keys := allKeys()
	seen := make(map[schemas.BifrostContextKey]bool, len(keys))
	for _, k := range keys {
		require.Falsef(
			t,
			seen[k],
			"context key %q duplicated — every key must be unique",
			string(k),
		)
		seen[k] = true
	}
	require.Len(t, seen, len(keys))
}

// TestKeyValues locks in the exact string value of every constant. This is the
// regression guard for accidental constant-value edits (e.g., dropping the
// "revenium-bifrost." prefix or renaming "key-hash" to "keyHash"). Phase 2/3
// hook bodies and the spike all assume these exact literals.
func TestKeyValues(t *testing.T) {
	cases := []struct {
		name string
		key  schemas.BifrostContextKey
		want string
	}{
		{"KeyHash", KeyHash, "revenium-bifrost.key-hash"},
		{"KeyRequestStart", KeyRequestStart, "revenium-bifrost.request-start"},
		{"KeyStreamState", KeyStreamState, "revenium-bifrost.stream-state"},
		{"KeyDryRunDecision", KeyDryRunDecision, "revenium-bifrost.dry-run-decision"},
		{"KeyShortCircuited", KeyShortCircuited, "revenium-bifrost.short-circuited"},
		// Phase 3 Plan 03-02 — KeyRequestType (Delta D).
		{"KeyRequestType", KeyRequestType, "revenium-bifrost.request-type"},
		// quick-260617-vhq — requested model/provider for the block event.
		{"KeyRequestModel", KeyRequestModel, "revenium-bifrost.request-model"},
		{"KeyRequestProvider", KeyRequestProvider, "revenium-bifrost.request-provider"},
		// Phase 2 D-19 — twelve KeyHeader* literals.
		{"KeyHeaderKeyHash", KeyHeaderKeyHash, "revenium-bifrost.header.key-hash"},
		{"KeyHeaderSubscriberID", KeyHeaderSubscriberID, "revenium-bifrost.header.subscriber-id"},
		{"KeyHeaderSubscriberEmail", KeyHeaderSubscriberEmail, "revenium-bifrost.header.subscriber-email"},
		{"KeyHeaderOrgName", KeyHeaderOrgName, "revenium-bifrost.header.organization-name"},
		{"KeyHeaderOrgID", KeyHeaderOrgID, "revenium-bifrost.header.organization-id"},
		{"KeyHeaderProductName", KeyHeaderProductName, "revenium-bifrost.header.product-name"},
		{"KeyHeaderProductID", KeyHeaderProductID, "revenium-bifrost.header.product-id"},
		{"KeyHeaderTraceID", KeyHeaderTraceID, "revenium-bifrost.header.trace-id"},
		{"KeyHeaderTaskType", KeyHeaderTaskType, "revenium-bifrost.header.task-type"},
		{"KeyHeaderAgent", KeyHeaderAgent, "revenium-bifrost.header.agent"},
		{"KeyHeaderSubscriptionID", KeyHeaderSubscriptionID, "revenium-bifrost.header.subscription-id"},
		{"KeyHeaderResponseQualityScore", KeyHeaderResponseQualityScore, "revenium-bifrost.header.response-quality-score"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, string(tc.key))
		})
	}
}
