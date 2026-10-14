DO $$ BEGIN RAISE EXCEPTION 'Restore a complete backup before downgrading reconciliation observation storage'; END $$;
