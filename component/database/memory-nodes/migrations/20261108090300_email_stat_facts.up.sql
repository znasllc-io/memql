-- The email statistics facts (docs/superpowers/specs/
-- 2026-11-08-site-and-email-statistics-design.md, section 5, D10 and D18).
--
-- A SIDE TABLE, NOT GRAPH ROWS. The Email Overview's figures are grouped
-- counts by day, organization, campaign, rule, journey and audience, and the
-- query language cannot group counts by time over graph rows; hand-written
-- SQL over the graph is the measured hazard the design rules out. So the
-- scheduled rollup (component/emailstats) walks the campaign rows THROUGH THE
-- ENGINE and records here one row per countable fact, which the read groups
-- with ordinary SQL.
--
-- ONE ROW PER (kind, source_id). source_id is the record the fact is about:
-- the delivery for a send and its outcomes and for a unique open or click, the
-- engagement event for an open or a click, the consent event for a
-- subscription change. The primary key is what makes a rerun, an overlapping
-- walk and two replicas' runs count nothing twice: every insert is ON CONFLICT
-- DO NOTHING.
--
-- Every id is BARE. account_id is empty for a row older than organization
-- attribution; such facts are counted only for staff reading every
-- organization. campaign_id, rule_id, journey_id and audience_id are empty
-- where the fact belongs to none.
--
-- day is the UTC day of the fact (D5), written as a date by the rollup and
-- never derived from a timestamp in the session's time zone. Facts are kept
-- 400 days (D4); the rollup deletes older ones itself, so the same code runs
-- on TimescaleDB and on plain Postgres.
CREATE TABLE IF NOT EXISTS email_stat_fact (
    kind        TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    account_id  TEXT NOT NULL DEFAULT '',
    campaign_id TEXT NOT NULL DEFAULT '',
    rule_id     TEXT NOT NULL DEFAULT '',
    journey_id  TEXT NOT NULL DEFAULT '',
    audience_id TEXT NOT NULL DEFAULT '',
    day         DATE NOT NULL,
    PRIMARY KEY (kind, source_id)
);

CREATE INDEX IF NOT EXISTS email_stat_fact_account_day_idx ON email_stat_fact (account_id, day);

CREATE INDEX IF NOT EXISTS email_stat_fact_day_idx ON email_stat_fact (day);

--bun:split

-- How far each source's walk has read: the newest row.createdAt of the last
-- page whose facts were written, per source concept, never past the clock of
-- the run that wrote it. The next run starts five minutes before it, so a row
-- written late with an earlier time is still read. A source with no row here
-- has never been walked, and its first run starts 400 days back, or where
-- email_stat_walk_start moved it (20261108090400).
CREATE TABLE IF NOT EXISTS email_stat_watermark (
    source  TEXT PRIMARY KEY,
    through TIMESTAMPTZ NOT NULL
);
