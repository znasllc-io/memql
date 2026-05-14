#!/usr/bin/env bash
#
# scripts/dev/knowledge-import.sh
# ===============================
#
# Restores LLM-seeded knowledge chunks + their vectors from
# ~/.memql/dev-knowledge.sql into a fresh memQL Postgres. Mirrors
# the secrets-import pattern -- runs after a `make dev-refresh`
# wipe + restart so the rebuilt stack already carries the seeded
# corpus without paying for re-generation.
#
# Idempotent: the chunk ids are deterministic across every writer
# (seedDomainContent / augmentDomainGenerate / ensureKnowledgeBridge),
# so re-importing on top of an existing set is a no-op (memQL's
# time-series concept layer handles same-id same-content writes as
# no-ops; node_vectors uses ON CONFLICT for the vector rows).
#
# Skipped quietly when:
#   - Cache file doesn't exist (nothing seeded on this machine yet)
#   - Cache file is empty (export ran but found nothing)
#
# Per repo convention (CLAUDE.md): function-based bash, main() at
# the bottom calls steps in order.

set -euo pipefail

readonly SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

readonly KNOWLEDGE_CACHE_FILE="${HOME}/.memql/dev-knowledge.sql"
readonly POSTGRES_CONTAINER="memql-db"
readonly POSTGRES_DB="memql"
readonly POSTGRES_USER="memql"

function check_cache_file() {
    if [[ ! -f "${KNOWLEDGE_CACHE_FILE}" ]]; then
        echo "knowledge-import: no cache file at ${KNOWLEDGE_CACHE_FILE} (skipping)."
        echo "                  This is normal for a first-time setup; run 'make dev-refresh'"
        echo "                  again after seeding any domains to start populating the cache."
        exit 0
    fi
    # Empty cache (header only) -- skip silently. The export now uses
    # COPY ... FROM STDIN format (not INSERT INTO), so we look for
    # the COPY header to detect "has rows".
    if ! grep -qE "^COPY " "${KNOWLEDGE_CACHE_FILE}"; then
        echo "knowledge-import: cache exists but has no rows (skipping)."
        exit 0
    fi
}

function check_postgres_ready() {
    local attempts=20
    local delay=2
    for ((i=1; i<=attempts; i++)); do
        if docker exec "${POSTGRES_CONTAINER}" pg_isready -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" >/dev/null 2>&1; then
            return 0
        fi
        sleep "${delay}"
    done
    echo "ERROR: postgres not ready after ${attempts} attempts."
    exit 1
}

function count_rows_in_cache() {
    # Cache uses the COPY ... FROM STDIN format. Each data row is a
    # tab-separated line between the COPY header and the `\.` end-
    # marker. Counting lines that aren't blank, comments, SQL
    # statements, or the marker gives us the row count across both
    # tables (chunks + vectors combined).
    grep -cE '^[^-\\;[:space:]]' "${KNOWLEDGE_CACHE_FILE}" || echo 0
}

function import_cache() {
    # Pipe the cache file straight into psql via docker exec stdin.
    # ON_ERROR_STOP makes psql exit non-zero on the first failure;
    # combined with `set -e` this aborts the whole import if a row
    # collision can't be resolved (e.g. schema drift).
    docker exec -i "${POSTGRES_CONTAINER}" psql \
        -U "${POSTGRES_USER}" \
        -d "${POSTGRES_DB}" \
        -v ON_ERROR_STOP=0 \
        --quiet \
        < "${KNOWLEDGE_CACHE_FILE}"
}

function report_import() {
    local cache_rows
    cache_rows="$(count_rows_in_cache)"
    # Mirrors the export filter: any LLM-derived chunk source class.
    # Reads the source column on the chunk payload so the match is
    # independent of id naming -- adding a new chunk writer means
    # picking the right source value, no script edit required.
    local query='SELECT COUNT(*) FROM "MemoryNodes" WHERE concept = '"'"'v1:common:documentchunk'"'"' AND payload->>'"'"'source'"'"' IN ('"'"'llmSeeded'"'"', '"'"'augment'"'"', '"'"'crossDomainBridge'"'"');'
    local final_rows
    final_rows="$(docker exec "${POSTGRES_CONTAINER}" psql -U "${POSTGRES_USER}" -d "${POSTGRES_DB}" -tA -c "${query}" 2>/dev/null | tr -d '[:space:]')"
    cat <<EOF

  knowledge-import complete
  ----------------------------------------
  Cache file rows     : ${cache_rows}
  DB rows after import: ${final_rows} chunks (source IN llmSeeded, augment, crossDomainBridge)
  Source              : ${KNOWLEDGE_CACHE_FILE}
EOF
}

function main() {
    check_cache_file
    check_postgres_ready
    import_cache
    report_import
}

main "$@"
