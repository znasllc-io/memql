-- How an email statistics walk carries on when one run cannot read all it
-- must (component/emailstats/rollup.go, "A burst the overlap cannot hold"
-- and "A walk whose pages outgrow a read").
--
-- reread_from: how far a walk got that stopped at its page bound while still
-- reading AGAIN the five minutes before its watermark. The next walk starts
-- there rather than five minutes before the watermark, so a burst of more
-- rows than one run reads in those five minutes is read in parts, each run
-- carrying on from the last, where before every run began it again and
-- failed at the same bound for good. It is never past the watermark, and once
-- the watermark is more than the overlap past it, it changes nothing.
--
-- read_until: how far a walk that has a watermark reads after one of its
-- pages took longer than a read may. The walk reads the graph as it stood at
-- that instant (asOf), which bounds what each page collapses to the versions
-- written between the walk's start and that instant; each further timeout
-- moves it half way back towards the start, and the walk that reads its last
-- page clears it, so the next walk reads to the clock again. Where before such
-- a walk stalled, its figures null, a Warn on every run.
--
-- In the watermark's row because both are written with it, in the one
-- transaction that records a page, and read with it at the start of a walk.
ALTER TABLE email_stat_watermark ADD COLUMN IF NOT EXISTS reread_from TIMESTAMPTZ;

--bun:split

ALTER TABLE email_stat_watermark ADD COLUMN IF NOT EXISTS read_until TIMESTAMPTZ;
