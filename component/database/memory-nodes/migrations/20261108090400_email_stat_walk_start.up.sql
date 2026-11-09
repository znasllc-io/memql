-- Where a source's email statistics walk starts, when an earlier first walk
-- could not read its first page (component/emailstats/rollup.go, "A first walk
-- that cannot read its first page"); and how far each source has been walked,
-- for the statistics read.
--
-- A first walk reads every version of a source written in the last 400 days,
-- and on a cluster with that much history its first page can outlast the
-- request pool's 8 s statement timeout. A failed page writes no watermark, so
-- every run would start that walk over from 400 days back and fail the same
-- way, and the source's statistics would never begin. Instead each timeout
-- moves the next first walk's start half way to the clock, down to a day, and
-- records it here, so a new cron leader carries on from it rather than from
-- 400 days back. A source with no row starts its first walk 400 days back.
--
-- A ROW BESIDE THE WATERMARK, NOT IN IT. A row of email_stat_watermark says
-- where the walk has read to; a moved start says the walk will never read
-- what is before it, so the read takes those days to be unmeasured for the
-- source's figures (component/emailstats/statistics_coverage.go).
CREATE TABLE IF NOT EXISTS email_stat_walk_start (
    source    TEXT PRIMARY KEY,
    starts_at TIMESTAMPTZ NOT NULL
);

--bun:split

-- TWO INSTANTS PER SOURCE, DECIDED BY TWO CLOCKS (rollup.go, "Never past the
-- clock"). `through` is where the next walk resumes, less the overlap: the
-- newest row.createdAt it wrote past, stamped by the rows' writers and only
-- capped at the cron leader's clock. `walked_through` is how far the source has
-- been walked, which the statistics read counts a figure as measured by: the
-- newest row too, or, once a walk has read its last page, the leader's clock as
-- it asked for that page. Kept in one column, a leader whose clock runs fast
-- would move where the next walk resumes past rows not yet written, and those
-- rows would never be read while their days read as measured; kept apart, the
-- leader's clock decides only what the read counts as measured.
--
-- In this table rather than one of its own because both are written together,
-- in the one transaction that records a page's facts, and read by the walk and
-- the read from the same row. `through` is NULL for a source walked with no row
-- yet to resume from, which is the "walked, nothing there" a never-walked
-- source, with no row here at all, must not be mistaken for.
ALTER TABLE email_stat_watermark ADD COLUMN IF NOT EXISTS walked_through TIMESTAMPTZ;

--bun:split

ALTER TABLE email_stat_watermark ALTER COLUMN through DROP NOT NULL;

--bun:split

-- A watermark written before the split was both instants at once.
UPDATE email_stat_watermark SET walked_through = through WHERE walked_through IS NULL AND through IS NOT NULL;
