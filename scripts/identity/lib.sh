#!/usr/bin/env bash
# scripts/identity/lib.sh
# Shared helpers for the identity-binary build pipeline.
#
# Sourced by sibling scripts (build-assets.sh, etc). Keep this file
# small — anything specific to a single script lives in that script.

# log <level> <msg>
log() {
  local level="$1"
  shift
  printf '%s identity-build: %s\n' "[$level]" "$*" >&2
}

# log_info / log_warn / log_error are convenience wrappers.
log_info()  { log "info"  "$*"; }
log_warn()  { log "warn"  "$*"; }
log_error() { log "error" "$*"; }

# require_cmd <name>
# Aborts the script if the named command isn't on PATH.
require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    log_error "$cmd is not on PATH"
    return 1
  fi
}
