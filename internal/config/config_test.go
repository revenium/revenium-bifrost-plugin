package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// clearEnv removes every REVENIUM_* env var so each test starts from a clean
// slate. t.Setenv("VAR", "") sets the var to empty, NOT unsets it, so we use
// t.Setenv to set then Unsetenv via Setenv("") is sufficient for our checks
// (Load uses strings.TrimSpace then `== ""`). We use t.Setenv exclusively so
// cleanup is automatic at test end.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"REVENIUM_METERING_API_KEY",
		"REVENIUM_METERING_BASE_URL",
		"REVENIUM_DEBUG",
		"REVENIUM_DRY_RUN",
	} {
		t.Setenv(k, "")
	}
}

// TestLoad_RequiresAPIKey asserts CONFIG-01: missing or whitespace-only
// REVENIUM_METERING_API_KEY produces a non-nil error mentioning the var
// name (so operators see a clear hint, not a cryptic load failure).
func TestLoad_RequiresAPIKey(t *testing.T) {
	cases := []struct {
		name      string
		apiKey    string
		wantError bool
	}{
		{"unset", "", true},
		{"whitespace only", "   ", true},
		{"tab-and-space", "\t  \n", true},
		{"non-empty", "sk-real-key", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("REVENIUM_METERING_API_KEY", tc.apiKey)

			cfg, err := Load()
			if tc.wantError {
				require.Error(t, err)
				require.Nil(t, cfg)
				require.Contains(t, err.Error(), "REVENIUM_METERING_API_KEY is required")
			} else {
				require.NoError(t, err)
				require.NotNil(t, cfg)
				require.Equal(t, strings.TrimSpace(tc.apiKey), cfg.APIKey)
			}
		})
	}
}

// TestLoad_BaseURLDefault asserts CONFIG-01 fallback: when
// REVENIUM_METERING_BASE_URL is unset the config falls back to
// DefaultMeteringBaseURL.
func TestLoad_BaseURLDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("REVENIUM_METERING_API_KEY", "sk-x")

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, DefaultMeteringBaseURL, cfg.BaseURL)
}

// TestLoad_BaseURLNormalization asserts trailing slash on the URL path is
// stripped, so callers can concatenate "/v2/api/..." paths without producing
// "//" sequences.
func TestLoad_BaseURLNormalization(t *testing.T) {
	clearEnv(t)
	t.Setenv("REVENIUM_METERING_API_KEY", "sk-x")
	t.Setenv("REVENIUM_METERING_BASE_URL", "https://example.com/")

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, "https://example.com", cfg.BaseURL)
}

// TestLoad_BaseURLRejectsScheme asserts non-http(s) schemes produce a
// validation error mentioning "scheme".
func TestLoad_BaseURLRejectsScheme(t *testing.T) {
	clearEnv(t)
	t.Setenv("REVENIUM_METERING_API_KEY", "sk-x")
	t.Setenv("REVENIUM_METERING_BASE_URL", "ftp://x.com")

	cfg, err := Load()
	require.Error(t, err)
	require.Nil(t, cfg)
	require.Contains(t, err.Error(), "scheme")
}

// TestLoad_BaseURLRejectsMissingHost asserts a URL without a host fails
// validation. "https:///path" parses successfully but has Host=="", which
// would silently produce broken outbound requests later.
func TestLoad_BaseURLRejectsMissingHost(t *testing.T) {
	clearEnv(t)
	t.Setenv("REVENIUM_METERING_API_KEY", "sk-x")
	t.Setenv("REVENIUM_METERING_BASE_URL", "https:///")

	cfg, err := Load()
	require.Error(t, err)
	require.Nil(t, cfg)
	require.Contains(t, strings.ToLower(err.Error()), "host")
}

// TestParseBool locks in the truthy set per the sibling Revenium Go
// middleware repo convention (case-insensitive after trim): "true", "1",
// "yes", "on". Everything else, including the empty string, is false.
func TestParseBool(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"  true  ", true},
		{"1", true},
		{"yes", true},
		{"YES", true},
		{"on", true},
		{"ON", true},
		{"", false},
		{"   ", false},
		{"false", false},
		{"0", false},
		{"no", false},
		{"off", false},
		{"garbage", false},
		{"truthy", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, parseBool(tc.in))
		})
	}
}

// TestLoad_DebugDryRunFlags walks the four-cell matrix of REVENIUM_DEBUG
// vs REVENIUM_DRY_RUN truthy/falsy combinations and asserts the resulting
// Config booleans match.
func TestLoad_DebugDryRunFlags(t *testing.T) {
	cases := []struct {
		name       string
		debugEnv   string
		dryRunEnv  string
		wantDebug  bool
		wantDryRun bool
	}{
		{"both unset", "", "", false, false},
		{"debug=true only", "true", "", true, false},
		{"dry-run=true only", "", "true", false, true},
		{"both true", "true", "true", true, true},
		{"both false strings", "false", "false", false, false},
		{"debug=1 dry-run=yes", "1", "yes", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("REVENIUM_METERING_API_KEY", "sk-x")
			t.Setenv("REVENIUM_DEBUG", tc.debugEnv)
			t.Setenv("REVENIUM_DRY_RUN", tc.dryRunEnv)

			cfg, err := Load()
			require.NoError(t, err)
			require.NotNil(t, cfg)
			require.Equal(t, tc.wantDebug, cfg.Debug)
			require.Equal(t, tc.wantDryRun, cfg.DryRun)
		})
	}
}

// TestLoad_BudgetCheckTimeoutIsThreeSeconds locks in the v1 contract: the
// budget-check timeout is always 3 * time.Second (matching the Python
// guardrail's httpx.Timeout(3.0)). CONFIG-03 reserves the env-overridable
// form for v2 (OPS-01); v1 ignores any environmental override.
func TestLoad_BudgetCheckTimeoutIsThreeSeconds(t *testing.T) {
	clearEnv(t)
	t.Setenv("REVENIUM_METERING_API_KEY", "sk-x")

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, 3*time.Second, cfg.BudgetCheckTimeout)
	require.Equal(t, DefaultBudgetCheckTimeout, cfg.BudgetCheckTimeout)
}
