-- The website statistics rollup folds each request once
-- (component/sitetraffic/rollup.go and fold.go, "Incremental, with a
-- two-hour tail"). Until 2026-11-09 every five-minute run recomputed today --
-- and yesterday until 02:00 -- from the whole request log; measured at ten
-- million requests a day over fifty sites, each of the day's three statements
-- took 47 to 52 s against a 60 s bound, and a run spent two and a half
-- minutes of database time every five. Now a request is folded into its day's
-- SETTLED state once it is two hours old, and each run reads only the last
-- two hours again. The four state tables below hold an open day's settled
-- state; a day's state goes when the day is final, except edge_stats_value_hour,
-- which the 24-hour top lists read until the day after.
--
-- PLAIN TABLES, as the rollup tables are (20261108090200): the rollup deletes
-- what it no longer needs itself, so the same code runs on TimescaleDB and on
-- plain Postgres.

-- How far each open day's settled fold has read: every request served before
-- settled_through is in the day's state. A day with no row has not been
-- folded at all, and its fold starts at its midnight.
CREATE TABLE IF NOT EXISTS edge_stats_progress (
    day             DATE        PRIMARY KEY,
    settled_through TIMESTAMPTZ NOT NULL
);

--bun:split

-- An open day's settled figures per site and UTC hour (0-23): what each
-- published hourly row is made of, with the tail's figures added. visitors is
-- the hour's distinct visitors; first_visitors and last_visitors count each
-- visitor once, in the hour of their first and of their last page view of the
-- day so far, which is what lets a 24-hour read add the end of one day to the
-- start of the next without counting anybody twice.
CREATE TABLE IF NOT EXISTS edge_stats_settled_hour (
    site_id        TEXT     NOT NULL,
    day            DATE     NOT NULL,
    hour           SMALLINT NOT NULL,
    measured       BOOLEAN  NOT NULL,
    visitors       BIGINT   NOT NULL,
    first_visitors BIGINT   NOT NULL,
    last_visitors  BIGINT   NOT NULL,
    visits         BIGINT   NOT NULL,
    bounces        BIGINT   NOT NULL,
    page_views     BIGINT   NOT NULL,
    requests       BIGINT   NOT NULL,
    errors         BIGINT   NOT NULL,
    not_found      BIGINT   NOT NULL,
    bytes          BIGINT   NOT NULL,
    bot_requests   BIGINT   NOT NULL,
    duration_hist  BIGINT[] NOT NULL,
    PRIMARY KEY (site_id, day, hour)
);

--bun:split

-- One row per visitor of an open day: what the next fold needs to tell a page
-- view that continues a visit from one that starts one, a visitor new to an
-- hour or to a value from one already counted there. hours has bit h set for
-- each UTC hour with one of their page views; last_at is their last folded
-- page view; visit_* is their open visit. seen holds a 64-bit hash of each
-- (dimension, value) they have been counted under (hashtextextended), and
-- seen_last the last hour of each. The visitor id is the daily hash the
-- request log already stores (D1); nothing here can name a person, and the
-- row goes when the day is final.
CREATE TABLE IF NOT EXISTS edge_stats_visitor (
    site_id       TEXT        NOT NULL,
    day           DATE        NOT NULL,
    visitor       TEXT        NOT NULL,
    hours         INTEGER     NOT NULL,
    last_at       TIMESTAMPTZ NOT NULL,
    visit_start   TIMESTAMPTZ NOT NULL,
    visit_views   INTEGER     NOT NULL,
    visit_sourced BOOLEAN     NOT NULL,
    seen          BIGINT[]    NOT NULL,
    seen_last     SMALLINT[]  NOT NULL,
    PRIMARY KEY (site_id, day, visitor)
);

--bun:split

-- An open day's settled figures per site, dimension, value and UTC hour: each
-- value's visitors by the hour they first and last viewed it, its visits by
-- the hour they started and its page views by hour. A day's top list adds the
-- hours up; the 24-hour top list adds the end of one day (by last hour) to the
-- start of the next (by first hour). Kept through the day after the day, for
-- that list.
CREATE TABLE IF NOT EXISTS edge_stats_value_hour (
    site_id        TEXT     NOT NULL,
    day            DATE     NOT NULL,
    dimension      TEXT     NOT NULL,
    value          TEXT     NOT NULL,
    hour           SMALLINT NOT NULL,
    first_visitors BIGINT   NOT NULL,
    last_visitors  BIGINT   NOT NULL,
    visits         BIGINT   NOT NULL,
    page_views     BIGINT   NOT NULL,
    PRIMARY KEY (site_id, day, dimension, value, hour)
);

--bun:split

-- Each published day's visitors by the UTC hour of their first and of their
-- last page view: 24 counts each, NULL where visitors is. The 24-hour read
-- counts a window's visitors as the visitors last seen in its first day's
-- hours plus those first seen in its second day's, exactly: the visitor hash
-- changes at midnight (D1), so nobody is in both. A day the rollup wrote
-- before these columns has none, and is folded again once if a 24-hour read
-- can still reach it (rollup.go, planRollup).
ALTER TABLE edge_stats_daily ADD COLUMN IF NOT EXISTS first_hour_visitors BIGINT[];

--bun:split

ALTER TABLE edge_stats_daily ADD COLUMN IF NOT EXISTS last_hour_visitors BIGINT[];

--bun:split

-- The current 24-hour window's top lists per site: the hundred values of
-- each dimension with the most visitors in the window and an (other) row for
-- the rest, as a day's are. Written by every run for the window that ends at
-- the next hour; the rows of any other window are deleted with it.
CREATE TABLE IF NOT EXISTS edge_stats_window_top (
    site_id    TEXT        NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    dimension  TEXT        NOT NULL,
    value      TEXT        NOT NULL,
    visitors   BIGINT      NOT NULL,
    visits     BIGINT      NOT NULL,
    page_views BIGINT      NOT NULL,
    PRIMARY KEY (site_id, window_end, dimension, value)
);

--bun:split

-- Each month's top lists per site, folded from the daily top rows of its
-- final days: a hundred values per dimension and an (other) row, as a day's
-- are. A read of whole months sums these instead of every day's rows (a staff
-- read of twelve months over two hundred sites summed 44 million daily rows
-- and took 7.6 s against the request pool's 8 s statement limit).
CREATE TABLE IF NOT EXISTS edge_stats_monthly_top (
    site_id    TEXT   NOT NULL,
    month      DATE   NOT NULL,
    dimension  TEXT   NOT NULL,
    value      TEXT   NOT NULL,
    visitors   BIGINT NOT NULL,
    visits     BIGINT NOT NULL,
    page_views BIGINT NOT NULL,
    PRIMARY KEY (site_id, month, dimension, value)
);

--bun:split

-- The days edge_stats_monthly_top covers: written with each month's fold, in
-- its transaction, so a read takes a month's rows for exactly these days and
-- every other day's from edge_stats_daily_top.
CREATE TABLE IF NOT EXISTS edge_stats_month_day (
    day DATE PRIMARY KEY
);
