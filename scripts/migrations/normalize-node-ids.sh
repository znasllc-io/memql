#!/usr/bin/env bash
set -euo pipefail

# Script: scripts/migrations/normalize-node-ids.sh
# Purpose: One-time, idempotent normalization of stored node ids to the
#          canonical {concept}:{shortId} form (#2439).
#
# Two legacy/malformed classes are handled:
#   1. Partition-prefixed ids ("<partition>:v1:...") -- the leading
#      partition segment was retired in #56 phase 6, but ad-hoc
#      composers kept producing the old form for a while. REWRITTEN
#      (strip everything before the version segment) in both
#      "MemoryNodes" and node_vectors, skipping rows whose normalized
#      target already exists (reported as conflicts for manual review).
#   2. Compound (colon-bearing) shortIds ("v1:x:y:<short>:extra:...")
#      -- REPORTED ONLY. Each such row needs a per-case decision (the
#      writer bug is fixed by the tightened id.ValidateShortId), so the
#      script never rewrites these automatically.
#
# Report-only by default; --apply --confirm=normalize-node-ids mutates.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/capability.sh"

cap_init "migrations.normalize-node-ids" "Normalize stored node ids to canonical {concept}:{shortId}; report compound shortIds."
cap_spec_param "database-url" "postgres connection URL (default: the local k3d dev database)"
cap_spec_param "apply"        "perform the rewrite (flag; default: report only)"
cap_spec_param "confirm"      "required with --apply: the exact phrase 'normalize-node-ids'"

#=============================================================================
# CONFIGURATION
#=============================================================================

DEFAULT_DB_URL="postgres://memql:memql_dev@localhost:5432/memql"

# First segment is not a version segment -> legacy partition prefix.
LEGACY_PREFIX_RE='^[^:]+:v[0-9]+:'
# Canonical concept (version:domain:entity) followed by a shortId that
# itself contains a colon -> compound shortId.
COMPOUND_SHORTID_RE='^v[0-9]+:[^:]+:[^:]+:[^:]*:'

#=============================================================================
# FUNCTIONS
#=============================================================================

function require_prerequisites() {
    command -v psql >/dev/null 2>&1 || cap_fail 4 "psql is not installed"
    psql "$DB_URL" -qAt -c "SELECT 1" >/dev/null 2>&1 \
        || cap_fail 4 "cannot connect to database: $DB_URL"
}

function scalar() {
    psql "$DB_URL" -qAt -c "$1"
}

function report_counts() {
    LEGACY_NODES="$(scalar "SELECT COUNT(*) FROM \"MemoryNodes\" WHERE id ~ '$LEGACY_PREFIX_RE'")"
    LEGACY_VECTORS="$(scalar "SELECT COUNT(*) FROM node_vectors WHERE id ~ '$LEGACY_PREFIX_RE'")"
    COMPOUND_NODES="$(scalar "SELECT COUNT(*) FROM \"MemoryNodes\" WHERE id ~ '$COMPOUND_SHORTID_RE'")"
    cap_info "legacy partition-prefixed ids: MemoryNodes=$LEGACY_NODES node_vectors=$LEGACY_VECTORS"
    cap_info "compound (colon-bearing) shortIds: MemoryNodes=$COMPOUND_NODES (report-only)"
    if [[ "$COMPOUND_NODES" != "0" ]]; then
        cap_warn "sample compound-shortId rows (concept, id):"
        psql "$DB_URL" -qAt -c "SELECT concept || ' ' || id FROM \"MemoryNodes\" WHERE id ~ '$COMPOUND_SHORTID_RE' LIMIT 10" >&2
    fi
}

function apply_rewrite() {
    cap_step "rewriting legacy partition-prefixed ids"
    # Skip rows whose normalized target already exists at the same PK
    # position; those are conflicts a human resolves.
    REWRITTEN_NODES="$(scalar "
        WITH moved AS (
            UPDATE \"MemoryNodes\" m
            SET id = regexp_replace(m.id, '^[^:]+:(v[0-9]+:)', '\\1')
            WHERE m.id ~ '$LEGACY_PREFIX_RE'
              AND NOT EXISTS (
                  SELECT 1 FROM \"MemoryNodes\" t
                  WHERE t.id = regexp_replace(m.id, '^[^:]+:(v[0-9]+:)', '\\1')
                    AND t.\"createdAt\" = m.\"createdAt\"
              )
            RETURNING 1
        ) SELECT COUNT(*) FROM moved")"
    REWRITTEN_VECTORS="$(scalar "
        WITH moved AS (
            UPDATE node_vectors v
            SET id = regexp_replace(v.id, '^[^:]+:(v[0-9]+:)', '\\1')
            WHERE v.id ~ '$LEGACY_PREFIX_RE'
              AND NOT EXISTS (
                  SELECT 1 FROM node_vectors t
                  WHERE t.id = regexp_replace(v.id, '^[^:]+:(v[0-9]+:)', '\\1')
                    AND t.vector_field = v.vector_field
              )
            RETURNING 1
        ) SELECT COUNT(*) FROM moved")"
    CONFLICT_NODES="$(scalar "SELECT COUNT(*) FROM \"MemoryNodes\" WHERE id ~ '$LEGACY_PREFIX_RE'")"
    CONFLICT_VECTORS="$(scalar "SELECT COUNT(*) FROM node_vectors WHERE id ~ '$LEGACY_PREFIX_RE'")"
    cap_info "rewritten: MemoryNodes=$REWRITTEN_NODES node_vectors=$REWRITTEN_VECTORS"
    if [[ "$CONFLICT_NODES" != "0" || "$CONFLICT_VECTORS" != "0" ]]; then
        cap_warn "conflicting rows left in legacy form (target id already exists): MemoryNodes=$CONFLICT_NODES node_vectors=$CONFLICT_VECTORS"
    fi
    [[ "$REWRITTEN_NODES" != "0" || "$REWRITTEN_VECTORS" != "0" ]] && cap_changed
    return 0
}

function emit_result() {
    cap_result_set_raw legacyPrefixedNodes "${LEGACY_NODES:-0}"
    cap_result_set_raw legacyPrefixedVectors "${LEGACY_VECTORS:-0}"
    cap_result_set_raw compoundShortIdNodes "${COMPOUND_NODES:-0}"
    cap_result_set_raw rewrittenNodes "${REWRITTEN_NODES:-0}"
    cap_result_set_raw rewrittenVectors "${REWRITTEN_VECTORS:-0}"
    cap_result_set_raw conflictNodes "${CONFLICT_NODES:-0}"
    cap_result_set_raw conflictVectors "${CONFLICT_VECTORS:-0}"
    cap_result_set_raw applied "$( [[ -n "$APPLY" ]] && echo true || echo false )"
}

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"

    DB_URL="$(cap_param database-url "$DEFAULT_DB_URL")"
    APPLY="$(cap_flag apply)"
    CONFIRM="$(cap_param confirm "")"

    require_prerequisites
    report_counts

    if [[ -n "$APPLY" ]]; then
        cap_confirm_or_die "$CONFIRM" "normalize-node-ids"
        apply_rewrite
        # Recount after the rewrite so the envelope reflects the end state.
        report_counts
    fi

    emit_result
    cap_ok
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
