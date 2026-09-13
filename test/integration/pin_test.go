//go:build integration

// pin_test.go anchors `github.com/stretchr/testify/require` into go.mod's
// require block via a single trivial sub-test. Mirrors the role of
// package_pin.go (which anchors bifrost/core) but for the test-only
// testify dep — testify cannot be referenced from a non-_test.go file
// without violating the package partition contract.
//
// Without this file, `go mod tidy` would strip testify v1.11.1 from
// go.mod's require block because Plan 04-03 Task 1 does not yet have
// integration_test.go (its 4 TestWire_* functions land in Task 2). The
// strip would violate Task 1's acceptance criterion that go.mod requires
// testify v1.11.1, and the next `go vet -tags=integration .` would re-
// surface the "go: updates to go.mod needed" warning.
//
// When Task 2 lands integration_test.go with its require.NoError /
// require.Equal call sites, this pin file becomes structurally redundant
// — but harmless. Leaving it in place documents the testify dep as an
// intentional pin (NOT a stray test) for future readers.

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPinTestify is a no-op sub-test whose ONLY job is to consume the
// testify import so go.mod's require block keeps testify v1.11.1 even
// when the future integration_test.go has not yet been authored.
func TestPinTestify(t *testing.T) {
	require.True(t, true, "trivial — see file-level comment for the pinning rationale")
}
