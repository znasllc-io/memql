DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_input_archive_schedules WHERE enabled OR lease_until>clock_timestamp()) THEN
        RAISE EXCEPTION 'release input archive scheduling is still enabled or admitted';
    END IF;
END $$;
DROP TABLE release_input_archive_schedules;
ALTER TABLE release_candidate_archives DROP COLUMN inputs_released_at;
