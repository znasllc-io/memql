-- Irreversible, and deliberately a no-op rather than a guess.
--
-- The up migration drops `onlyStages` from v1:pipelines:run rows, and the
-- stage list it held is gone once dropped. Rolling back means running the
-- engine version that still declared the field, which reads a run without it
-- as a run of every stage its event selects -- the value an absent key always
-- had. So the down direction leaves the rows as they are.

SELECT 1;
