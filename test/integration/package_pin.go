// package_pin.go is an UNCONDITIONAL (no build-tag) source file in the
// test/integration sub-module whose ONLY job is to pin
// `github.com/maximhq/bifrost/core/schemas` into go.mod's require block so
// that `go mod tidy` does not strip it. Without this file, fake_openai.go +
// fake_revenium.go + integration_test.go are ALL `//go:build integration`-
// tagged — when `go vet . 2>&1` (no tag) runs against the sub-module it
// would otherwise have no Go file to compile, but it WOULD insist on
// reconciling go.mod against the (empty) untagged-file dependency set,
// stripping the bifrost/core require.
//
// Mirrors the spike/contract pattern: spike.go is similarly untagged and
// imports schemas to anchor bifrost/core in the spike sub-module's go.mod.
// In our case there is no equivalent production source file to anchor (the
// .so lives in the parent module), so this file's sole responsibility is
// the pin.
//
// Per D-04 sub-module isolation: this file imports schemas but does NOT
// import any production-internal package — the parent module's internal
// subtree is structurally unreachable from a sibling sub-module.

package main

import (
	"github.com/maximhq/bifrost/core/schemas"
)

// pinSchemas anchors github.com/maximhq/bifrost/core into go.mod's require
// block. The dereference of a no-op enum value is the smallest possible
// reference that survives compiler dead-code elimination. Verified
// 2026-05-24 against core/v1.5.11/schemas: RequestCancelled is the named
// constant for the cancelled-request RequestType (a stable identifier; if
// schemas ever renames this, the sub-module's go.mod tidy will fail and
// surface the breaking change before the matrix sees it).
var pinSchemas = schemas.RequestCancelled

// main is a no-op stub so `go build -tags=integration ./...` produces a
// linkable binary. The integration sub-module is NEVER built as an
// executable in production — its only build target is `go test -c` /
// `go test -tags=integration` against the test binary. The stub exists
// because `package main` without a main function fails `go build ./...`
// with the diagnostic "function main is undeclared in the main package".
func main() {}
