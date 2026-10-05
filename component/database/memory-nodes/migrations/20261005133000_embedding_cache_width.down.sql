-- Refuse rollback if a wider/different-width cache still exists; never discard it.
ALTER TABLE embedding_cache ALTER COLUMN embedding TYPE vector(1536);
