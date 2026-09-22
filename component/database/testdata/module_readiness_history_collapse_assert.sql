DO $$
BEGIN
  IF EXISTS (
    (SELECT * FROM "MemoryNodes" EXCEPT ALL SELECT * FROM expected_rows)
    UNION ALL
    (SELECT * FROM expected_rows EXCEPT ALL SELECT * FROM "MemoryNodes")
  ) THEN
    RAISE EXCEPTION 'The collapse must keep exactly the newest version of each readiness row and every row of every other concept';
  END IF;
  -- Named separately from the set comparison above, because "the newest one
  -- survived" and "a verdict survived at all" fail for different reasons and
  -- a reader of the failure needs to know which.
  IF NOT EXISTS (
    SELECT 1 FROM "MemoryNodes"
     WHERE id = 'v1:platform:moduleReadiness:ai--bff-a'
       AND concept = 'v1:platform:moduleReadiness'
       AND "createdAt" = '2026-09-13T10:30:00Z'
  ) THEN
    RAISE EXCEPTION 'The surviving version must be the newest by createdAt, not the last inserted';
  END IF;
  IF (SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:platform:moduleReadiness') <> 3 THEN
    RAISE EXCEPTION 'Three readiness rows had versions; exactly three must survive, one each';
  END IF;
END $$;
