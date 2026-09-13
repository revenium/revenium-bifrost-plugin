.PHONY: bifrost-host build ci clean fmt lint print-bifrost-tag test tools

GO ?= go
GOTOOLCHAIN ?= go1.26.2
BIFROST_TRANSPORTS_TAG ?= transports/v1.5.3
SO_NAME := revenium-bifrost.so

# WARNING: do not pass strip-symbol or strip-DWARF ldflags to the build below.
# Stripping the symbol table breaks plugin.Lookup at load time (Pitfall 5).
# Native -buildmode=plugin only; do not cross-compile.
#
# BUILD-06 (ROADMAP §Phase 4 Success Criterion 5):
# Cross-compilation is structurally impossible for `-buildmode=plugin`.
# pkg.go.dev/plugin documents the platform constraint, and
# golang/go#17832 explains why: CGO + plugin mode
# requires the linker to resolve symbols against the TARGET's libc/dyld at
# build time, and the runtime ABI must match the host toolchain exactly.
# Setting `GOOS=darwin GOARCH=arm64` on a linux/amd64 host does NOT produce
# a loadable darwin/arm64 plugin; it produces a broken object. QEMU emulation
# of a foreign-arch Linux userspace is verified-broken for plugin mode too
# (04-RESEARCH.md §Pitfall 16) — never reach for `qemu-user-static` as an
# escape hatch. For each target OS/arch, build natively on a matching runner.
# `.github/workflows/release.yml` is the documented native-build path:
# 4 GitHub-hosted runners (ubuntu-24.04 + ubuntu-24.04-arm + macos-15-intel +
# macos-14) per BUILD-02, each invoking THIS target on the matching runner.
build:
	CGO_ENABLED=1 GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) build -buildmode=plugin -trimpath -o $(SO_NAME) .

# Scope test target to ./internal/... — ./... would try to build the top-level
# `package main`, which requires -buildmode=plugin. Top-level pluginsanity
# tests (build-tag `pluginsanity`) run via dedicated invocations in CI.
test:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) test ./internal/...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w -s .

clean:
	rm -f $(SO_NAME)
	rm -rf bin/ dist/ .bifrost-tmp/

tools:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

# Build the Bifrost HTTP host harness from source at the tag matching core/v1.5.11.
# Same Go 1.26.2 toolchain as the plugin guarantees `plugin.Open` succeeds.
#
# Upstream gotcha (verified 2026-05-21 against transports/v1.5.3): a bare
# `go install github.com/maximhq/bifrost/transports/bifrost-http@<tag>` fails
# because (a) Go refuses the `transports/v1.5.3` nested-tag form as an
# `@version` arg, and (b) `main.go` uses `//go:embed all:ui` to embed a `ui/`
# subtree that is NOT shipped inside the published Go-module distribution —
# the UI assets are only produced by the upstream's npm packaging pipeline.
# Workaround: copy the module-cache source into a writable temp dir, drop a
# placeholder `ui/index.html` to satisfy the embed directive, and `go build`
# from there with our pinned toolchain. Phase-1 spike does not exercise UI.
BIFROST_BUILD_TMP := .bifrost-tmp/bifrost-transports
bifrost-host:
	mkdir -p bin
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) mod download github.com/maximhq/bifrost/core@v1.5.11
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) mod download github.com/maximhq/bifrost/transports@v1.5.3
	rm -rf $(BIFROST_BUILD_TMP)
	mkdir -p $(BIFROST_BUILD_TMP)
# The trailing `/.` on the cp SOURCE below is deliberate: GNU cp (Linux) given a
# trailing `/` copies the DIRECTORY ITSELF, while BSD cp (macOS) copies its
# CONTENTS. The `/.` form means "contents" on both. Without it, Linux nests the
# tree one level deeper, the mkdir below then materializes an EMPTY
# bifrost-http/, and the go build fails with `no Go files`.
	cp -R $$($(GO) env GOMODCACHE)/github.com/maximhq/bifrost/transports@v1.5.3/. $(BIFROST_BUILD_TMP)/
	chmod -R u+w $(BIFROST_BUILD_TMP)
	mkdir -p $(BIFROST_BUILD_TMP)/bifrost-http/ui
	printf '%s\n' '<!-- ui stub for Phase-1 source build; Plan 06 spike does not exercise UI -->' > $(BIFROST_BUILD_TMP)/bifrost-http/ui/index.html
	cd $(BIFROST_BUILD_TMP)/bifrost-http && CGO_ENABLED=1 GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) build -trimpath -o $(PWD)/bin/bifrost-http .
	$(GO) version -m bin/bifrost-http | head -1

# CI pipeline: lint fast, then test, then prove the plugin builds.
ci: lint test build

# Echo the BIFROST_TRANSPORTS_TAG verbatim (D-35.1 single source of truth).
# Phase 4 Plan 04-02's cache-key step in ci.yml consumes this via shell
# substitution: `tag=$(make print-bifrost-tag)`. The `@` prefix suppresses
# command echo so the captured value is JUST the tag string with no extra
# noise. No dependencies; no side effects; safe to call from any CI step.
print-bifrost-tag:
	@echo $(BIFROST_TRANSPORTS_TAG)
