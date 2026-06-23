#!/usr/bin/env bash
#
# scripts/dev/knowledge-export.sh
# ===============================
#
# Pulls every LLM-seeded knowledge chunk + its vector out of the
# running memQL Postgres and writes them to a local cache at
# ~/.memql/dev-knowledge.sql. Mirrors the secrets-export pattern --
# the goal is to survive a `make dev-refresh` (which wipes the DB)
# without burning fresh LLM tokens to regenerate the seed corpus.
#
# What gets exported:
#
#   - "MemoryNodes" rows where concept = 'v1:common:documentchunk'
#     AND payload->>'source' is one of the LLM-derived classes:
#       * 'llmSeeded'         -- catalog Tier-A/B bodies, Tier-B
#                                disclaimers, Tier-C Wikipedia
#                                chunks (everything the
#                                seedDomainContent pipeline writes).
#       * 'augment'           -- chunks the chat 'Analyze for
#                                training' action wrote (topic-
#                                focused additions to an existing
#                                domain).
#       * 'crossDomainBridge' -- bridge content generated when an
#                                agent has 2+ domains.
#   - node_vectors rows for those same ids (so retrieval works
#     after the import without re-embedding).
#
# Skipped: appStructure (CoPresent UI corpus regenerates from
# in-binary text on startup) + fileUpload (user-uploaded chunks
# regenerate from the source file the user re-uploads).
#
# Filter is on the chunk's source column, NOT the id prefix.
# Adding a new chunk writer means picking the right source value
# at insert time; this script keeps working without an edit.
#
# Per repo convention (CLAUDE.md): function-based bash, main() at
# the bottom calls steps in order.

set -euo pipefail

readonly KNOWLEDGE_CACHE_DIR="${HOME}/.memql"
readonly KNOWLEDGE_CACHE_FILE="${KNOWLEDGE_CACHE_DIR}/dev-knowledge.sql"
readonly POSTGRES_CONTAINER="memql-db"
readonly POSTGRES_DB="memql"
readonly POSTGRES_USER="memql"

function postgres_running() {
    docker ps --filter "name=^${POSTGRES_CONTAINER}$" --filter "status=running" --format '{{.Names}}' | grep -q "${POSTGRES_CONTAINER}"
}

function ensure_cache_dir() {
    mkdir -p "${KNOWLEDGE_CACHE_DIR}"
}

# SQL fragment shared between the count, the DELETE, and the COPY.
# Reads payload->>'source' so the filter is independent of any id
# naming convention. Encoded as a function so a future addition
# (e.g. a new LLM-derived source value) only edits one place.
function llm_derived_filter() {
    echo "concept = 'v1:common:documentchunk' AND payload->>'source' IN ('llmSeeded', 'augment', 'crossDomainBridge')"
}

function count_seeded_rows() {
    local query
    query="SELECT COUNT(*) FROM \"MemoryNodes\" WHERE $(llm_derived_filter);"
    docker exec "${POSTGRES_CONTAINER}" psql -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" -tA -c "${query}" 2>/dev/null | tr -d '[:space:]'
}

function export_chunks_and_vectors() {
    # COPY-based export/import. We could try to construct INSERT
    # statements via format(%L, payload::text) but jsonb columns
    # round-trip badly that way -- the chunk's `text` field contains
    # an embedded JSON envelope (`<!--seed:{...}-->`) whose
    # backslashes get double-escaped through (jsonb -> text -> SQL
    # literal -> psql parse), producing invalid JSON on import. COPY
    # handles all encoding natively.
    #
    # Rationale for ditching pg_dump:
    #
    # 1. "MemoryNodes" is a TimescaleDB HYPERTABLE. pg_dump on the
    #    parent returns 0 INSERTs because the actual data lives in
    #    _timescaledb_internal._hyper_* chunk tables that pg_dump
    #    doesn't auto-include. Querying through SQL (which the
    #    planner routes through chunks) works uniformly.
    #
    # 2. pg_dump emits `INSERT INTO public.node_vectors ...` (with
    #    schema prefix). The earlier grep for `^INSERT INTO
    #    node_vectors` missed those silently.
    #
    # Idempotency: we DELETE existing seed-* rows before COPYing
    # the cache in. With deterministic chunk ids, re-importing
    # cleanly replaces whatever was there with the cached snapshot.

    {
        echo "-- memQL knowledge cache, generated $(date -u +%FT%TZ)"
        echo "-- Restored by knowledge-import.sh during 'make dev-refresh'"
        echo "-- See scripts/dev/knowledge-export.sh for the export query."
        echo ""

        # Disable trigger firing during restore so we don't accidentally
        # fire CDC hooks for inserts that are really restorations.
        echo "SET session_replication_role = replica;"
        echo "BEGIN;"
        echo ""

        local filter
        filter="$(llm_derived_filter)"

        echo "-- Idempotent reset: clear any existing LLM-derived rows so the"
        echo "-- COPY below replaces rather than collides on the deterministic"
        echo "-- chunk ids. Filter targets payload->>'source' so the script"
        echo "-- doesn't have to know about id naming conventions."
        echo "DELETE FROM \"MemoryNodes\" WHERE ${filter};"
        echo "DELETE FROM node_vectors WHERE id IN (SELECT id FROM \"MemoryNodes\" WHERE ${filter});"
        echo ""

        echo "-- MemoryNodes: documentchunk rows tagged source IN (llmSeeded, augment, crossDomainBridge)"
        echo "COPY \"MemoryNodes\" (partition, id, \"createdAt\", \"createdBy\", schema, payload, metadata, type, concept) FROM STDIN;"
        docker exec "${POSTGRES_CONTAINER}" psql \
            -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" \
            --quiet -tA \
            -c "\\copy (SELECT partition, id, \"createdAt\", \"createdBy\", schema, payload, metadata, type, concept FROM \"MemoryNodes\" WHERE ${filter}) TO STDOUT" \
            2>/dev/null
        echo "\\."
        echo ""

        echo "-- node_vectors: vectors for the same chunks (joined back via id)"
        echo "COPY node_vectors (partition, id, concept, vector_field, embedding, created_at, updated_at) FROM STDIN;"
        docker exec "${POSTGRES_CONTAINER}" psql \
            -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" \
            --quiet -tA \
            -c "\\copy (SELECT v.partition, v.id, v.concept, v.vector_field, v.embedding, v.created_at, v.updated_at FROM node_vectors v WHERE v.id IN (SELECT id FROM \"MemoryNodes\" WHERE ${filter})) TO STDOUT" \
            2>/dev/null
        echo "\\."
        echo ""

        echo "COMMIT;"
        echo "SET session_replication_role = DEFAULT;"
    } > "${KNOWLEDGE_CACHE_FILE}.tmp"

    mv "${KNOWLEDGE_CACHE_FILE}.tmp" "${KNOWLEDGE_CACHE_FILE}"
}

function report_export() {
    local count
    count="$(count_seeded_rows)"
    local file_size
    file_size="$(du -h "${KNOWLEDGE_CACHE_FILE}" | cut -f1)"
    cat <<EOF

  knowledge-export complete
  ----------------------------------------
  Source rows         : ${count} chunks (source IN llmSeeded, augment, crossDomainBridge)
  Cache file          : ${KNOWLEDGE_CACHE_FILE}
  Cache size          : ${file_size}
  Restored by         : scripts/dev/knowledge-import.sh (auto-runs in 'make dev-refresh')
EOF
}

function main() {
    ensure_cache_dir
    # First-run / cold-start path: no postgres container means there's
    # nothing to cache. Treat it the same as "no rows": write an empty
    # cache so the matching import is a no-op, and exit cleanly. The
    # ERROR-and-exit-1 behavior was wrong for `make dev-refresh` which
    # legitimately runs when there's nothing to back up yet.
    if ! postgres_running; then
        echo "knowledge-export: no running postgres yet (first-run); nothing to cache."
        echo "-- memQL knowledge cache (empty -- no postgres at $(date -u +%FT%TZ))" > "${KNOWLEDGE_CACHE_FILE}"
        exit 0
    fi
    local count
    count="$(count_seeded_rows)"
    if [[ -z "${count}" || "${count}" == "0" ]]; then
        echo "knowledge-export: no LLM-seeded chunks in DB (nothing to cache)."
        # Still write an empty cache file so import is a no-op.
        echo "-- memQL knowledge cache (empty -- nothing to cache at $(date -u +%FT%TZ))" > "${KNOWLEDGE_CACHE_FILE}"
        exit 0
    fi
    export_chunks_and_vectors
    report_export
}

main "$@"
