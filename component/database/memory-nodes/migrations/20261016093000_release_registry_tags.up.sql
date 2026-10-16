-- One immutable version tag belongs to one native publication intent across
-- owners and replicas. Retiring a candidate does not release its reservation.
ALTER TABLE release_publication_intents ADD COLUMN IF NOT EXISTS registry_tag_key text
    CHECK (registry_tag_key IS NULL OR registry_tag_key ~ '^sha256:[0-9a-f]{64}$');
CREATE UNIQUE INDEX IF NOT EXISTS release_registry_tag_reservation
    ON release_publication_intents(registry_tag_key) WHERE registry_tag_key IS NOT NULL;
