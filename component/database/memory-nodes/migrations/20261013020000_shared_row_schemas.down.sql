-- Dropping the catalog would destroy the schemas needed to read stored rows.
DO $$ BEGIN
    RAISE EXCEPTION 'shared row schemas require a full-schema backup restore to downgrade; refusing to discard referenced schemas';
END $$;
