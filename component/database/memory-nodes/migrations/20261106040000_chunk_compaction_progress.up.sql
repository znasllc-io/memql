-- Mutable maintenance state for the closed-chunk compaction
-- (component/retention/compact.go); never a versioned graph row.
--
-- One row per closed "MemoryNodes" / "SecretMemoryNodes" chunk the job has
-- looked at, keyed by the chunk's schema-qualified name. A measurement (the
-- live bytes a full heap scan found) is reused while the chunk's size and its
-- insert/update/delete counter still read what they read when it was taken, so
-- an unchanged chunk is never scanned twice; a retention delete moves the
-- counter and the next run measures again. The attempt columns hold the last
-- rewrite's outcome and the consecutive failures a chunk backs off by.
CREATE TABLE chunk_compaction_progress (
    chunk text PRIMARY KEY,
    hypertable text NOT NULL DEFAULT '',
    measured_at timestamptz,
    measured_total_bytes bigint,
    measured_index_bytes bigint,
    measured_changes bigint,
    live_bytes bigint,
    live_rows bigint,
    measure_failed_at timestamptz,
    attempted_at timestamptz,
    outcome text NOT NULL DEFAULT '',
    detail text NOT NULL DEFAULT '',
    failures integer NOT NULL DEFAULT 0,
    compacted_at timestamptz
);
