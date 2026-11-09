-- Where a source's email statistics walk starts, when an earlier first walk
-- could not read its first page (component/emailstats/rollup.go, "A first walk
-- that cannot read its first page").
--
-- A first walk reads every version of a source written in the last 400 days,
-- and on a cluster with that much history its first page can outlast the
-- request pool's 8 s statement timeout. A failed page writes no watermark, so
-- every run would start that walk over from 400 days back and fail the same
-- way, and the source's statistics would never begin. Instead each failure
-- moves the next first walk's start half way to the clock, down to a day, and
-- records it here, so a new cron leader carries on from it rather than from
-- 400 days back. A source with no row starts its first walk 400 days back.
--
-- A ROW BESIDE THE WATERMARK, NOT IN IT. A row of email_stat_watermark says
-- the walk has read through its instant, which is what the statistics read
-- counts a figure as measured by; a moved start says the walk will never read
-- what is before it, so the read takes these days to be unmeasured for the
-- source's figures (component/emailstats/statistics_coverage.go).
CREATE TABLE IF NOT EXISTS email_stat_walk_start (
    source    TEXT PRIMARY KEY,
    starts_at TIMESTAMPTZ NOT NULL
);
