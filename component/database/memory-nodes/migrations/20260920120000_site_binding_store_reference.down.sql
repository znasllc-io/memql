-- Irreversible, and deliberately a no-op rather than a guess.
--
-- The up migration replaces a copied {storeDomain, storefrontTokenRef} with a
-- reference to the row that already held both. Writing the copy back would
-- re-create the duplication this epic removed, and it could only be written
-- back from the store row -- which is to say from the reference, which is to
-- say it was never lost.
--
-- Rolling this back means rolling back the engine version that made the
-- change, at which point component/edge reads the copy again and the rows it
-- reads no longer carry one. That is a version rollback, not a data
-- migration, and the repair is to re-run the older engine's own writers.

SELECT 1;
