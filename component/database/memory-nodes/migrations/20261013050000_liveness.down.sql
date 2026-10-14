DO $$ BEGIN RAISE EXCEPTION 'Restore a complete backup before downgrading liveness storage'; END $$;
