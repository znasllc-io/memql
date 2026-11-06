DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM release_installation_proposals WHERE landed_commit IS NOT NULL) THEN
  RAISE EXCEPTION 'cannot remove recorded installation owner landings';
 END IF;
END $$;
ALTER TABLE release_installation_proposals DROP COLUMN landed_commit;
