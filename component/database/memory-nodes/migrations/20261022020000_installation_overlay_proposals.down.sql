DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM release_installation_proposals) THEN
  RAISE EXCEPTION 'installation overlay proposal history must be retained';
 END IF;
END $$;
DROP TABLE release_installation_proposals;
