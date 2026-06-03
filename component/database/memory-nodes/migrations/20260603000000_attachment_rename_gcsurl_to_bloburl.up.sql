-- Rename payload field gcsURL -> blobUrl on v1:common:attachment rows.
--
-- Post GCS->Azure migration (memql#801 / PR #803) the field holds an
-- Azure Blob Storage URL, not a gs:// URI. The DSL concept, shape, and
-- all Go readers/writers are renamed in the same commit (memql#810).
--
-- Strategy: use jsonb_set to copy the value from the old key and then
-- remove the old key with the - operator. Both steps are done in a
-- single UPDATE so the row is never in a state where both keys exist
-- simultaneously.
--
-- Idempotent: rows that already carry `blobUrl` (none today, but safe
-- for re-runs) are left untouched by the WHERE guard; rows that lack
-- `gcsURL` also pass through untouched.
--
-- The down migration reverses by swapping blobUrl -> gcsURL.
--
-- NOTE: no `--bun:split` before the first statement (a leading
-- comment-only segment makes bun emit an empty query and abort the
-- migration -- see memql#570 / migration 20260520000000).

UPDATE "MemoryNodes"
SET payload = (payload - 'gcsURL') || jsonb_build_object('blobUrl', payload->>'gcsURL')
WHERE concept = 'v1:common:attachment'
  AND payload ? 'gcsURL'
  AND NOT (payload ? 'blobUrl');
