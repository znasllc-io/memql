-- Down for 20260921000000_module_readiness_history_collapse.
--
-- A DELIBERATE NO-OP. The up migration removed restatements of verdicts that
-- had not changed. Nothing in this database can reconstruct one, and a down
-- that invented versions to fill the gap would fabricate a history of
-- evaluations that never happened -- a record of a cluster checking itself at
-- moments it did not, which is worse than the absence.
--
-- There is also nothing to roll back TO. The up changes no shape and drops no
-- field: every node's current verdict is still there, still the newest version
-- of its own row, and every reader of the concept (`asOf latest`) sees exactly
-- what it saw before. Rolling back the engine version that ran this needs no
-- data change at all.
--
-- This file exists because the pair is the convention and a missing Down reads
-- as an oversight rather than a decision.

SELECT 1;
