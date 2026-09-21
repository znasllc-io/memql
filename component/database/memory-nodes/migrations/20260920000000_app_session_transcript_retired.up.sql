-- Retired concept fields: the app session's flattened transcript
-- (epic memql#5396, task memql#5398).
--
--   v1:worker:appSession    transcript, transcriptBytes
--
-- THE MECHANISM IS memql#5199's: `additionalProperties: false` plus a
-- read-merge that validates the MERGED payload means any stored key its concept
-- no longer declares makes that row unwritable on its next write, with
-- `additionalProperties 'transcript' not allowed`. CI cannot see it -- the
-- db-tests lane runs against a fresh database -- so it appears first on a real
-- installation, as a write that fails on every boot.
--
-- WHERE THEY WENT. `transcript` held every AppSessionChunk flattened into one
-- 256 KiB string with the stream and the sequence discarded, and
-- `transcriptBytes` counted what went in, truncation included. Epic memql#5396
-- records what a session DID as rows of the work spine -- one v1:work:step and
-- one v1:work:observation per action -- and puts the model's prose in the
-- Library as one content-addressed file per session, named by the row's new
-- `transcriptFileId`. `transcriptTruncated` SURVIVES and is deliberately not
-- stripped here: it now says the transcript FILE does not hold the whole
-- output, which is still a fact the row must be able to state.
--
-- SCOPED BY CONCEPT, NEVER BY KEY NAME. `transcript` is a plausible field name
-- on anything that records output, and this migration must not reach a concept
-- that still declares one -- the rule `status` earned when it was retired on one
-- cluster concept while live on two others with 4,679 versions between them
-- (memql#5199).
--
-- APPEND-ONLY ROWS, EVERY VERSION -- a read-merge reads the newest, but the
-- older versions are what an audit walk returns, and a session's history is the
-- evidence somebody opens it for.
--
-- Idempotent: the `payload ?|` guard matches nothing on a cluster whose rows
-- were all written after the retirement.

UPDATE "MemoryNodes"
SET payload = payload - 'transcript' - 'transcriptBytes'
WHERE concept = 'v1:worker:appSession'
  AND payload ?| array['transcript', 'transcriptBytes'];
