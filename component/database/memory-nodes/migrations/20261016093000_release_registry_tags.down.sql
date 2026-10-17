DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_publication_intents WHERE registry_tag_key IS NOT NULL) THEN
        RAISE EXCEPTION 'Cannot discard durable registry tag reservations';
    END IF;
END $$;
DROP INDEX IF EXISTS release_registry_tag_reservation;
ALTER TABLE release_publication_intents DROP COLUMN IF EXISTS registry_tag_key;
