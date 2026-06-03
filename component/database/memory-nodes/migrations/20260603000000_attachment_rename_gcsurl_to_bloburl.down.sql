-- Reverse: rename payload field blobUrl -> gcsURL on v1:common:attachment rows.
-- Inverts the up migration (memql#810).

UPDATE "MemoryNodes"
SET payload = (payload - 'blobUrl') || jsonb_build_object('gcsURL', payload->>'blobUrl')
WHERE concept = 'v1:common:attachment'
  AND payload ? 'blobUrl'
  AND NOT (payload ? 'gcsURL');
