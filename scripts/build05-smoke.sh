#!/usr/bin/env bash
#
# scripts/build05-smoke.sh — BUILD-05 per-runner plugin.Open smoke entrypoint
# (Phase 4 D-35.2). Invoked once per matrix runner in .github/workflows/ci.yml,
# AFTER the codesign step (so the .so being exercised is the signed artifact
# on macOS) and BEFORE the integration test step (Pitfall 7 — smoke FIRST so
# each step starts from a clean process state; integration runs SECOND with
# its own bifrost-http instance).
#
# Flow:
#   1. Ensure revenium-bifrost.so exists; if not, run `make build`.
#   2. Ensure bin/bifrost-http exists; if not, run `make bifrost-host`.
#   3. Invoke the Go harness (scripts/build05-smoke-harness.go) via `go run`.
#      The harness handles plugin.Open via a bifrost-http subprocess, drives
#      one chat-completion curl, SIGTERMs, and prints telemetry lines.
#   4. Parse the harness stdout for BIFROST_LOG path + DELTA_SECONDS value.
#   5. Assert:
#        a. DELTA_SECONDS >= 1.0 (D-35.2 floor — production Cleanup does
#           NOT include the spike's deliberate 2s sleep; 1s is the safe
#           floor that still proves Bifrost honored the synchronous
#           Cleanup contract; spike captured 2.115383s for context).
#        b. bifrost.log contains the literal "revenium" plugin name (Init
#           log line — production GetName() returns "revenium" exactly).
#        c. bifrost.log contains at least one PreLLMHook / pre-llm log line
#           (DEBUG level fired by production plugin per BUDGET-01).
#        d. bifrost.log contains at least one Cleanup / cleanup log line
#           (exactly one Cleanup invocation expected; >= 1 occurrence is
#           the assertion to keep this tolerant of multi-line log shapes).
#   6. Exit 0 on all assertions pass; 1 on any failure.
#
# Local pre-merge usage:
#
#   ./scripts/build05-smoke.sh
#
# CI usage (.github/workflows/ci.yml step body):
#
#   ./scripts/build05-smoke.sh
#
# Bash version: requires bash 3.2+ (the macOS-shipped baseline). No bash 4-only
# features used (no associative arrays, no ${var^^}).

set -euo pipefail

# --- Step 1: ensure the .so exists. -----------------------------------------
if [ ! -f revenium-bifrost.so ]; then
  echo "build05-smoke: revenium-bifrost.so missing — running 'make build'..."
  make build
fi

# --- Step 2: ensure bin/bifrost-http exists. --------------------------------
if [ ! -f bin/bifrost-http ]; then
  echo "build05-smoke: bin/bifrost-http missing — running 'make bifrost-host'..."
  make bifrost-host
fi

# --- Step 3: invoke the harness. --------------------------------------------
# Capture stdout into a variable AND tee to terminal so CI logs preserve the
# telemetry lines verbatim. tee uses a temp file because bash's process
# substitution and `|` would each lose the exit code on the harness side.
HARNESS_OUT_FILE=$(mktemp -t build05-smoke-out.XXXXXX)
trap 'rm -f "$HARNESS_OUT_FILE"' EXIT

set +e
go run ./scripts/build05-smoke-harness.go \
  -bifrost-bin bin/bifrost-http \
  -plugin-so revenium-bifrost.so \
  | tee "$HARNESS_OUT_FILE"
HARNESS_EXIT=${PIPESTATUS[0]}
set -e

if [ "$HARNESS_EXIT" != "0" ]; then
  echo "FAIL: harness exited non-zero ($HARNESS_EXIT) — see stderr above."
  exit 1
fi

HARNESS_OUT=$(cat "$HARNESS_OUT_FILE")

# --- Step 4: extract telemetry lines from harness stdout. -------------------
# The harness prints lines like:
#   BIFROST_LOG: /tmp/build05-smoke-.../bifrost.log
#   SIGTERM_SENT_AT: 2026-05-24T...
#   PROCESS_EXITED_AT: 2026-05-24T...
#   DELTA_SECONDS: 1.234567
# We grep for the first occurrence of each key and take the value after the
# `: ` separator.
BIFROST_LOG=$(printf '%s\n' "$HARNESS_OUT" | grep -E '^BIFROST_LOG: '       | head -1 | sed -E 's/^BIFROST_LOG:[[:space:]]+//')
DELTA_SECONDS=$(printf '%s\n' "$HARNESS_OUT" | grep -E '^DELTA_SECONDS: '     | head -1 | sed -E 's/^DELTA_SECONDS:[[:space:]]+//')

if [ -z "$BIFROST_LOG" ]; then
  echo "FAIL: harness output did not contain a 'BIFROST_LOG: ' line."
  exit 1
fi
if [ -z "$DELTA_SECONDS" ]; then
  echo "FAIL: harness output did not contain a 'DELTA_SECONDS: ' line."
  exit 1
fi
if [ ! -f "$BIFROST_LOG" ]; then
  echo "FAIL: bifrost.log path '$BIFROST_LOG' does not exist on disk."
  exit 1
fi

# --- Step 5a: DELTA_SECONDS >= 1.0 (D-35.2 floor). --------------------------
# Bash's [ -gt ] cannot compare floats. Use awk for the float comparison.
# Also validate DELTA_SECONDS is numeric (a stray non-numeric value would
# silently `awk` to 0 — the explicit regex guards against that).
if ! printf '%s' "$DELTA_SECONDS" | grep -qE '^-?[0-9]+(\.[0-9]+)?$'; then
  echo "FAIL: DELTA_SECONDS '$DELTA_SECONDS' is not numeric."
  exit 1
fi

DELTA_OK=$(awk -v d="$DELTA_SECONDS" 'BEGIN { if (d+0 >= 1.0) print "yes"; else print "no" }')
if [ "$DELTA_OK" != "yes" ]; then
  echo "FAIL: SIGTERM-to-Cleanup delta '${DELTA_SECONDS}s' is below the 1s floor (D-35.2)."
  echo "      The Phase 1 spike captured 2.115383s with a deliberate 2s sleep in Cleanup;"
  echo "      the production plugin's Cleanup synchronously flushes its metering client"
  echo "      with no deliberate sleep but typically takes 50-500ms+resilience-flush time."
  echo "      A delta < 1s suggests Bifrost killed the process mid-Cleanup."
  echo ""
  echo "--- bifrost.log (last 80 lines) ---"
  tail -n 80 "$BIFROST_LOG" || true
  echo "--- end bifrost.log ---"
  exit 1
fi

# --- Step 5b: "revenium" appears in bifrost.log (Init log line). ------------
if ! grep -qi "revenium" "$BIFROST_LOG"; then
  echo "FAIL: bifrost.log does not contain the literal 'revenium' — plugin Init log line missing."
  echo "      Production plugin's GetName() returns the literal 'revenium'; absence in the log"
  echo "      suggests Init did not run (plugin.Open or Lookup failed)."
  echo ""
  echo "--- bifrost.log (last 80 lines) ---"
  tail -n 80 "$BIFROST_LOG" || true
  echo "--- end bifrost.log ---"
  exit 1
fi

# --- Step 5c: PreLLMHook log line present. ----------------------------------
# Tolerant grep — matches either "PreLLMHook" (exact symbol name) or
# "pre-llm" (slug form some loggers emit). DEBUG-level log lines fire from
# the production plugin's PreLLMHook per BUDGET-01.
if ! grep -qiE "PreLLMHook|pre-llm" "$BIFROST_LOG"; then
  echo "FAIL: bifrost.log does not contain 'PreLLMHook' or 'pre-llm' — pre-call hook did NOT fire."
  echo ""
  echo "--- bifrost.log (last 80 lines) ---"
  tail -n 80 "$BIFROST_LOG" || true
  echo "--- end bifrost.log ---"
  exit 1
fi

# --- Step 5d: Cleanup log line present (>= 1 occurrence). -------------------
CLEANUP_COUNT=$(grep -ciE "Cleanup|cleanup" "$BIFROST_LOG" || true)
if [ -z "$CLEANUP_COUNT" ] || [ "$CLEANUP_COUNT" -lt 1 ]; then
  echo "FAIL: bifrost.log does not contain a Cleanup log line — Cleanup did NOT fire on SIGTERM."
  echo ""
  echo "--- bifrost.log (last 80 lines) ---"
  tail -n 80 "$BIFROST_LOG" || true
  echo "--- end bifrost.log ---"
  exit 1
fi

# --- Step 6: success. -------------------------------------------------------
echo "BUILD-05 SMOKE PASS: delta=${DELTA_SECONDS}s"
exit 0
