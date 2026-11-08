-- The finalization record of the website statistics rollup (docs/superpowers/
-- specs/2026-11-08-site-and-email-statistics-design.md, section 5).
--
-- ONE ROW PER UTC DAY WHOSE STATISTICS ARE FINAL. The rollup
-- (component/sitetraffic/rollup.go) recomputes today on every run, and every
-- earlier day still in the request log that has no row here. A run that
-- recomputes a day at or after 02:00 UTC the following day writes its row, and
-- from then on the day is left as it is. So a day's last recompute comes after
-- every request served that day has been written, and an outage of any length,
-- up to the request log's own retention, heals on the next run instead of
-- leaving the day's figures short for good.
--
-- A PLAIN TABLE, like the rollup tables beside it: one small row per day, which
-- the rollup deletes with the day's daily row, after 400 days. So a final day
-- stays final while its figures are kept: a request dated on it that turns up
-- after the request log has dropped the day cannot reopen it.
CREATE TABLE IF NOT EXISTS edge_stats_final (
    day          DATE        PRIMARY KEY,
    finalized_at TIMESTAMPTZ NOT NULL
);
