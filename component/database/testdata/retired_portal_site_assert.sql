DO $$
BEGIN
  IF EXISTS (
    (SELECT * FROM "MemoryNodes" EXCEPT ALL SELECT * FROM expected_sites)
    UNION ALL
    (SELECT * FROM expected_sites EXCEPT ALL SELECT * FROM "MemoryNodes")
  ) THEN
    RAISE EXCEPTION 'Retirement must remove both old versions and preserve every unrelated row';
  END IF;
END $$;
