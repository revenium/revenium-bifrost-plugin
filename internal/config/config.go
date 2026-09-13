// Package config loads and validates the Revenium Bifrost plugin's runtime
// configuration from environment variables. Per CLAUDE.md "Configuration
// surface" the v1 contract is env-only — no fields from the Bifrost-side
// config.json plugin entry are consumed.
//
// Variables read:
//
//   - REVENIUM_METERING_API_KEY (required) — outgoing auth for both the
//     budget-check HTTP client and the metering SDK client. Missing or
//     whitespace-only value aborts Init with a descriptive error.
//   - REVENIUM_METERING_BASE_URL (optional) — base URL for budget-check
//     and metering. Falls back to DefaultMeteringBaseURL. Must parse as a
//     valid http(s) URL with a non-empty host; the trailing slash on the
//     path is stripped via normalizeBaseURL.
//   - REVENIUM_DEBUG (optional, truthy values: "true","1","yes","on" —
//     case-insensitive) — elevates the plog handler to LevelDebug.
//   - REVENIUM_DRY_RUN (optional, same truthy set) — Phase 2 will use
//     this to allow budget-block decisions through while still emitting
//     a "would_have_blocked" metering event.
//
// Load returns an immutable *Config; no setters are exported, so callers
// can pass the pointer freely without races.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// DefaultMeteringBaseURL is the fallback used when
	// REVENIUM_METERING_BASE_URL is unset. This is the confirmed Revenium
	// default production host and matches the revenium-go-sdk's own default.
	// Override it per-deployment via REVENIUM_METERING_BASE_URL.
	DefaultMeteringBaseURL = "https://api.revenium.ai"

	// DefaultBudgetCheckTimeout matches the Python middleware's
	// httpx.Timeout(3.0) and is the v1 contract for the budget-check
	// *http.Client overall timeout. NOT env-overridable in v1 —
	// CONFIG-03 reserves the env form for v2 (OPS-01).
	DefaultBudgetCheckTimeout = 3 * time.Second
)

// Config is the immutable, validated configuration constructed from the
// process environment. All fields are read-only after Load returns; no
// exported setters exist, so callers may pass the pointer freely.
type Config struct {
	// BaseURL is the (normalized) Revenium API base URL — no trailing
	// slash. Callers concatenate "/v2/api/..." paths directly.
	BaseURL string

	// APIKey is the value of REVENIUM_METERING_API_KEY after TrimSpace.
	// Required; Load returns a non-nil error if it ends up empty.
	APIKey string

	// Debug is true when REVENIUM_DEBUG is in the truthy set. Drives the
	// log/slog handler level.
	Debug bool

	// DryRun is true when REVENIUM_DRY_RUN is in the truthy set. Phase 2
	// PreLLMHook reads this to allow budget-blocks through while still
	// recording the would-have-blocked decision via KeyDryRunDecision.
	DryRun bool

	// BudgetCheckTimeout is the *http.Client overall timeout for the
	// budget-check call. Hard-pinned to DefaultBudgetCheckTimeout in v1.
	BudgetCheckTimeout time.Duration
}

// Load reads REVENIUM_* environment variables, validates them, and returns
// an immutable *Config. Returns a non-nil error (and nil Config) when
// required values are missing or when the base URL fails validation.
func Load() (*Config, error) {
	apiKey := strings.TrimSpace(os.Getenv("REVENIUM_METERING_API_KEY"))
	if apiKey == "" {
		return nil, errors.New("REVENIUM_METERING_API_KEY is required")
	}

	rawBaseURL := strings.TrimSpace(os.Getenv("REVENIUM_METERING_BASE_URL"))
	if rawBaseURL == "" {
		rawBaseURL = DefaultMeteringBaseURL
	}
	normalized, err := normalizeBaseURL(rawBaseURL)
	if err != nil {
		return nil, fmt.Errorf("REVENIUM_METERING_BASE_URL invalid: %w", err)
	}

	return &Config{
		BaseURL:            normalized,
		APIKey:             apiKey,
		Debug:              parseBool(os.Getenv("REVENIUM_DEBUG")),
		DryRun:             parseBool(os.Getenv("REVENIUM_DRY_RUN")),
		BudgetCheckTimeout: DefaultBudgetCheckTimeout,
	}, nil
}

// parseBool accepts "true", "1", "yes", "on" (case-insensitive, after
// trimming surrounding whitespace) as true. Everything else, including the
// empty string, is false. Matches the sibling revenium-middleware-google-go
// REVENIUM_DEBUG convention.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
