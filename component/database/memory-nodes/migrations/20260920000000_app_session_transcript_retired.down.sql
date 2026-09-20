-- Down for 20260920000000_app_session_transcript_retired.
--
-- A DELIBERATE NO-OP. The up migration deleted two payload keys; the values
-- they held are gone and nothing in this database can reconstruct them, so a
-- down that re-added the keys would write empty strings and zeroes where a
-- transcript used to be -- a fabricated record of somebody's agent run, which
-- is worse than the absence.
--
-- Rolling back the SCHEMA half is rolling back the release: re-declare
-- `transcript` and `transcriptBytes` on v1:worker:appSession and the rows are
-- writable again, carrying no transcript, which is the honest state after the
-- data is gone.
--
-- This file exists because the pair is the convention and a missing Down reads
-- as an oversight rather than a decision.

SELECT 1;
