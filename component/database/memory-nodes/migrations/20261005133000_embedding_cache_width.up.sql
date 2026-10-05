-- Exact-key TTL cache, never a nearest-neighbor index. Preserve existing rows
-- while accepting vectors from the configured model's actual width.
ALTER TABLE embedding_cache ALTER COLUMN embedding TYPE vector;
