-- Derived recovery state only. MemoryNodes remains the canonical history.
-- Writers append invalidations in their OWN transaction; they never wait for
-- a projector or take cross-run locks. Sequence order is NOT commit order.
CREATE TABLE IF NOT EXISTS work_run_heads (
    id text PRIMARY KEY,
    "createdAt" timestamptz NOT NULL,
    status text NOT NULL
);
CREATE INDEX IF NOT EXISTS work_run_heads_in_flight
    ON work_run_heads ("createdAt", id)
    WHERE status NOT IN ('succeeded', 'failed', 'cancelled', 'abandoned');
CREATE TABLE IF NOT EXISTS work_run_head_events (
    sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    id text NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS work_run_head_events_age ON work_run_head_events (enqueued_at);
CREATE TABLE IF NOT EXISTS work_run_head_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    cursor_id text NOT NULL DEFAULT '',
    backfill_complete boolean NOT NULL DEFAULT false
);
INSERT INTO work_run_head_state (singleton) VALUES (true) ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION enqueue_work_run_head() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO work_run_head_events (id)
        SELECT DISTINCT id FROM new_rows WHERE concept='v1:work:run';
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO work_run_head_events (id)
        SELECT DISTINCT id FROM old_rows WHERE concept='v1:work:run';
    ELSE
        INSERT INTO work_run_head_events (id)
        SELECT id FROM old_rows WHERE concept='v1:work:run'
        UNION SELECT id FROM new_rows WHERE concept='v1:work:run';
    END IF;
    RETURN NULL;
END
$$;
-- Transition tables coalesce a retention batch's many historical versions
-- into one event per ID. TimescaleDB >=2.18 supports them on hypertables.
CREATE TRIGGER work_run_head_insert AFTER INSERT ON "MemoryNodes"
    REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION enqueue_work_run_head();
CREATE TRIGGER work_run_head_update AFTER UPDATE ON "MemoryNodes"
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION enqueue_work_run_head();
CREATE TRIGGER work_run_head_delete AFTER DELETE ON "MemoryNodes"
    REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION enqueue_work_run_head();

-- A reset must make recovery visibly unavailable until a new bounded backfill
-- finishes. Administrative chunk drops/disabled triggers require the same
-- explicit reset; they are not part of the supported retention path.
CREATE OR REPLACE FUNCTION invalidate_work_run_heads() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    UPDATE work_run_head_state SET cursor_id = '', backfill_complete = false WHERE singleton;
    DELETE FROM work_run_heads;
    RETURN NULL;
END
$$;
DROP TRIGGER IF EXISTS work_run_head_reset ON "MemoryNodes";
CREATE TRIGGER work_run_head_reset AFTER TRUNCATE ON "MemoryNodes"
    FOR EACH STATEMENT EXECUTE FUNCTION invalidate_work_run_heads();
DROP TRIGGER IF EXISTS work_run_head_reset ON work_run_heads;
CREATE TRIGGER work_run_head_reset AFTER TRUNCATE ON work_run_heads
    FOR EACH STATEMENT EXECUTE FUNCTION invalidate_work_run_heads();
DROP TRIGGER IF EXISTS work_run_head_reset ON work_run_head_events;
CREATE TRIGGER work_run_head_reset AFTER TRUNCATE ON work_run_head_events
    FOR EACH STATEMENT EXECUTE FUNCTION invalidate_work_run_heads();

-- One bounded batch. Call in a REPEATABLE READ transaction, then read heads in
-- that SAME transaction only if ready=true. Commit not-ready progress too.
-- Bootstrap is resumable and deliberately outside the startup migration.
CREATE OR REPLACE FUNCTION refresh_work_run_heads(batch_size integer DEFAULT 1000)
RETURNS jsonb LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    state work_run_head_state%ROWTYPE;
    event_ids bigint[];
    run_ids text[];
    next_cursor text;
    pending integer;
    oldest timestamptz;
BEGIN
    IF batch_size < 1 OR batch_size > 2000 THEN
        RAISE EXCEPTION 'work head batch size must be between 1 and 2000';
    END IF;
    IF (SELECT count(*) FROM pg_trigger
        WHERE tgrelid = '"MemoryNodes"'::regclass AND NOT tgisinternal AND tgenabled IN ('O','A')
          AND ((tgname IN ('work_run_head_insert','work_run_head_update','work_run_head_delete') AND tgfoid='enqueue_work_run_head()'::regprocedure)
            OR (tgname='work_run_head_reset' AND tgfoid='invalidate_work_run_heads()'::regprocedure))) <> 4 THEN
        RAISE EXCEPTION 'work head capture triggers missing or disabled; repair and rebuild before recovery';
    END IF;
    -- A concurrent projector fails immediately. At REPEATABLE READ, a state
    -- row changed since our snapshot raises 40001; retry the WHOLE transaction.
    SELECT * INTO STRICT state FROM work_run_head_state WHERE singleton FOR UPDATE NOWAIT;
    IF NOT state.backfill_complete THEN
        SELECT array_agg(k.id ORDER BY k.id), max(k.id) INTO run_ids, next_cursor
        FROM (
            SELECT DISTINCT id FROM "MemoryNodes"
            WHERE concept='v1:work:run' AND id > state.cursor_id
            ORDER BY id LIMIT batch_size
        ) k;
        IF next_cursor IS NULL THEN
            UPDATE work_run_head_state SET backfill_complete=true WHERE singleton;
            state.backfill_complete := true;
        ELSE
            UPDATE work_run_head_state SET cursor_id=next_cursor WHERE singleton;
        END IF;
    ELSE
        SELECT array_agg(e.sequence), array_agg(DISTINCT e.id) INTO event_ids, run_ids
        FROM (SELECT sequence,id FROM work_run_head_events ORDER BY sequence LIMIT batch_size) e;
    END IF;
    IF run_ids IS NOT NULL THEN
        -- Both base and dirty-ID batches probe the existing latest-row index.
        -- Even a deleted/reinserted ID is resolved from canonical source here.
        DELETE FROM work_run_heads WHERE id = ANY(run_ids);
        INSERT INTO work_run_heads (id,"createdAt",status)
        SELECT n.id,n."createdAt",COALESCE(n.payload->>'status','')
        FROM unnest(run_ids) AS k(id)
        JOIN LATERAL (
            SELECT id,"createdAt",payload FROM "MemoryNodes"
            WHERE concept='v1:work:run' AND id=k.id
            ORDER BY "createdAt" DESC LIMIT 1
        ) n ON true;
    END IF;
    -- Never delete by a watermark: a lower sequence can commit AFTER this
    -- transaction and must remain visible to the next projector.
    DELETE FROM work_run_head_events WHERE sequence = ANY(event_ids);
    SELECT count(*) INTO pending FROM (SELECT 1 FROM work_run_head_events LIMIT 10001) q;
    SELECT enqueued_at INTO oldest FROM work_run_head_events ORDER BY enqueued_at LIMIT 1;
    RETURN jsonb_build_object('ready',state.backfill_complete AND pending=0,
        'backfillComplete',state.backfill_complete,
        'processedIds',COALESCE(cardinality(run_ids),0),
        'processedEvents',COALESCE(cardinality(event_ids),0),
        'pendingEventsAtLeast',pending,'oldestPendingAt',oldest);
END
$$;
