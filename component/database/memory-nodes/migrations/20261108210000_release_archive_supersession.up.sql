-- Two kinds of archive evidence share one journal. A 'catalog' row records the
-- signed catalog archival verified against. A 'supersession' row records an
-- approved candidate that never had a catalog, archived because a later or
-- equal release of every component it contains has one and every copy it
-- published was read back first (releaseArchiveWithoutCatalog). It has no
-- catalog digest by definition, so the digest is required exactly for the
-- catalog basis, and neither kind of row can pass for the other.
ALTER TABLE release_candidate_archives ALTER COLUMN catalog_digest DROP NOT NULL;
ALTER TABLE release_candidate_archives ADD COLUMN basis text NOT NULL DEFAULT 'catalog';
ALTER TABLE release_candidate_archives ADD CONSTRAINT release_candidate_archives_basis_check
    CHECK (basis IN ('catalog','supersession') AND ((basis = 'catalog') = (catalog_digest IS NOT NULL)));
