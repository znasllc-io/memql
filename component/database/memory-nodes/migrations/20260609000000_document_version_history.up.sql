-- Library document version history (memql#1228).
--
-- v1:library:documentVersion rows are an APPEND-ONLY, immutable content
-- history for a logical Library document: producing or editing a
-- document (memql#1229 user edit, memql#1231 assistant edit) or
-- restoring an earlier one (memql#1230) APPENDS a new version row with
-- the next monotonic versionNumber instead of overwriting. The artifact
-- index + backing generatedOutput always point at the latest version;
-- the full history stays retained behind them.
--
-- Storage / time-series choice:
--   Like every other concept, documentVersion rows live in the
--   "MemoryNodes" TimescaleDB hypertable (already created + partitioned
--   on createdAt in 20260324000000_initial_setup). So version history is
--   inherently time-partitioned -- no separate physical table, and the
--   engine's per-row authz + partition + mutation middleware apply
--   uniformly (the same decision the observability `invocation` concept
--   documents, except those rows are written by the observe runtime
--   directly while documentVersion rows go through the engine).
--
--   This migration therefore does NOT create a new table. It (a) adds a
--   composite index tuned for the version-history access pattern -- "all
--   versions of document X, newest first" -- scoped to the documentVersion
--   concept via a partial index, and (b) documents + (where TimescaleDB
--   is present and the hypertable is not already compression-enabled)
--   enables a LONG-WINDOW compression policy on MemoryNodes.
--
-- Retention choice (deliberate):
--   History is long-lived. We set NO scheduled retention drop -- versions
--   are never evicted on a timer (contrast the observability hypertable's
--   7-day retention). Compression after a long window (90 days) is the
--   only lifecycle applied, and only to the cold tail; recent history
--   stays uncompressed for fast edit/restore reads. This mirrors the
--   observability migration's compress-DO-block shape while deliberately
--   omitting any scheduled-eviction policy.
--
-- Cross-references:
--   * Concept surface:  dsl/library/concepts.memql (documentVersion)
--   * Edit handlers:    integrations/library/ (editDocument / restoreDocumentVersion)
--   * Base hypertable:  20260324000000_initial_setup.up.sql ("MemoryNodes")

-- Version-history access index. documentVersion rows are read as
-- "every version of one logical document" (queryDocumentVersions) +
-- "the latest version" (the edit/restore handlers). The logical
-- document id lives in payload->>'documentId'; ordering is by
-- versionNumber. A partial index keyed on (documentId, versionNumber DESC),
-- scoped to the concept, keeps the history read off a full scan without
-- bloating the index with every other concept's rows.
CREATE INDEX IF NOT EXISTS memory_nodes_document_version_history_idx
    ON "MemoryNodes" (
        (payload->>'documentId'),
        ((payload->>'versionNumber')::bigint) DESC
    )
    WHERE concept = 'v1:library:documentVersion';

--bun:split

-- Long-window compression on the MemoryNodes hypertable. Append-only
-- history is the canonical compressible workload: old versions are
-- read-rarely and never updated. Segment by `concept` so a single
-- concept's cold rows (e.g. one app's whole documentVersion tail)
-- decompress as a contiguous segment; order by createdAt DESC so the
-- newest cold rows decompress first. Compress only the 90-day-cold
-- tail -- recent history (the hot edit/restore path) stays uncompressed.
--
-- Guarded: only runs when TimescaleDB is present, MemoryNodes is a
-- hypertable, and compression isn't already configured (so re-running
-- migrations or a future MemoryNodes-compression migration is a no-op).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        IF EXISTS (
            SELECT 1 FROM timescaledb_information.hypertables
            WHERE hypertable_name = 'MemoryNodes'
        ) AND NOT EXISTS (
            SELECT 1 FROM timescaledb_information.compression_settings
            WHERE hypertable_name = 'MemoryNodes'
        ) THEN
            ALTER TABLE "MemoryNodes"
                SET (
                    timescaledb.compress,
                    timescaledb.compress_segmentby = 'concept',
                    timescaledb.compress_orderby   = '"createdAt" DESC'
                );
            PERFORM add_compression_policy('MemoryNodes', INTERVAL '90 days', if_not_exists => TRUE);
        END IF;
    ELSE
        RAISE NOTICE 'timescaledb not installed -- skipping MemoryNodes compression policy for document version history';
    END IF;
EXCEPTION WHEN OTHERS THEN
    -- Compression is an optimization, not a correctness requirement.
    -- A missing timescaledb_information view (older TS) or a server
    -- without the feature must not fail the migration -- the index
    -- above already provides the access-path guarantee.
    RAISE NOTICE 'skipping MemoryNodes compression policy: %', SQLERRM;
END
$$;
