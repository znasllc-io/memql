#!/usr/bin/env bash
# Evaluate a single policy with a JSON args literal and dump the
# structured trace tree.
#
# Phase 0 placeholder. The real debug runner lands in Phase 6 once
# engine.EvaluatePolicy + the trace infrastructure ship.
#
# Usage:
#   make policies-trace POLICY=avatarVendorChoice ARGS='{"expectedDurationMinutes":30}'
set -euo pipefail

function show_usage() {
  cat <<'EOF'
policies-trace POLICY=<name> ARGS='<json literal>'

Phase 0 placeholder. When Phase 3/6 lands, this script boots a
minimal engine, runs engine.EvaluatePolicy(<name>, <args>) with
@returns_trace, and prints the JSON trace tree.

For now it just echoes what it would do.
EOF
}

function main() {
  local policy="${1:-}"
  local args="${2:-{}}"
  if [[ -z "$policy" ]]; then
    show_usage
    exit 1
  fi
  cat <<EOF
policies-trace (Phase 0 placeholder)
  policy: $policy
  args:   $args
  -> would evaluate via engine.EvaluatePolicy and print the trace tree.
EOF
}

main "$@"
