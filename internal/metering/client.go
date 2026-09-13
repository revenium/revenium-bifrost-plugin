package metering

import (
	"fmt"

	revsdk "github.com/revenium/revenium-go-sdk/core/metering"

	"github.com/revenium/revenium-bifrost-plugin/internal/config"
)

// Client wraps a single revsdk.MeteringClient so the rest of the plugin
// codebase depends on this package's surface rather than on the SDK's
// concrete types directly. Phase 1 holds the wrapped client; Phase 3 hook
// bodies call (*Client).Send via this wrapper.
type Client struct {
	inner *revsdk.MeteringClient
}

// New constructs the underlying SDK MeteringClient using the Revenium API key
// and base URL from the immutable *config.Config. REAL construction per D-12 —
// not a stub. The SDK validates that APIKey is non-empty and returns an error
// otherwise; we surface that as a wrapped error so callers (Init) can return
// a descriptive top-level Init failure.
func New(cfg *config.Config) (*Client, error) {
	inner, err := revsdk.NewMeteringClient(revsdk.MeteringClientConfig{
		APIKey:  cfg.APIKey,
		BaseURL: cfg.BaseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("internal/metering: NewMeteringClient failed: %w", err)
	}
	return &Client{inner: inner}, nil
}

// Send is a pass-through to revsdk.MeteringClient.Send. The SDK's Send is
// fire-and-forget (no return value) — it spawns a goroutine, tracks it via
// the SDK's internal WaitGroup, and reports failures via the SDK's package
// logger. Phase 3 hook bodies invoke this from PostLLMHook and the
// HTTPTransportStreamChunkHook terminal-chunk branch.
//
// Per CLAUDE.md "Don't Hand-Roll" rule 1 we MUST NOT wrap Send in our own
// goroutine — that would race with the SDK's WaitGroup and cause lost
// metering events on shutdown.
func (c *Client) Send(p *revsdk.MeteringPayload) {
	c.inner.Send(p)
}

// Close is a synchronous pass-through to revsdk.MeteringClient.Close, which
// invokes Flush() under the hood and wg.Wait()s for every in-flight Send
// goroutine to complete. Cleanup() in plugin.go invokes this so CONTRACT-05
// (synchronous flush of in-flight metering events on shutdown) is satisfied
// in Phase 1.
//
// The SDK's Close always returns nil today; the error return is preserved
// here in case a future SDK version reports flush failures (e.g., timeout
// waiting for the goroutine pool to drain).
func (c *Client) Close() error {
	return c.inner.Close()
}
