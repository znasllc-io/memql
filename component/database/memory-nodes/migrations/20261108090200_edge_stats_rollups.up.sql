-- Rollup tables for a deployable's website statistics (docs/superpowers/specs/
-- 2026-11-08-site-and-email-statistics-design.md, section 5).
--
-- PLAIN TABLES, not hypertables and with no retention policy. The scheduled
-- rollup job deletes rows past retention itself, so the very same code runs on
-- TimescaleDB and on plain Postgres, and nothing here depends on an extension.
--
-- NULLABLE VISITOR COLUMNS. visitors, visits, bounces and page_views are NULL
-- when the period was not measured, which is every period before the edge
-- began classifying requests. NULL must read as "not measured", never as zero
-- visitors; the other counters are health counters every request has always
-- fed, so they are NOT NULL.
--
-- duration_hist is a 64-bucket log-scale histogram of response times (four
-- buckets per doubling from 0.5 ms, see component/sitetraffic/histogram.go).
-- A median of medians is not a median, so a percentile over any range is
-- merged from buckets and is accurate to about 10%.
CREATE TABLE IF NOT EXISTS edge_stats_hourly (
    site_id       TEXT        NOT NULL,
    hour          TIMESTAMPTZ NOT NULL,
    visitors      BIGINT,
    visits        BIGINT,
    bounces       BIGINT,
    page_views    BIGINT,
    requests      BIGINT      NOT NULL,
    errors        BIGINT      NOT NULL,
    not_found     BIGINT      NOT NULL,
    bytes         BIGINT      NOT NULL,
    bot_requests  BIGINT      NOT NULL,
    duration_hist BIGINT[]    NOT NULL,
    PRIMARY KEY (site_id, hour)
);

CREATE INDEX IF NOT EXISTS edge_stats_hourly_hour_idx ON edge_stats_hourly (hour);

--bun:split

CREATE TABLE IF NOT EXISTS edge_stats_daily (
    site_id       TEXT      NOT NULL,
    day           DATE      NOT NULL,
    visitors      BIGINT,
    visits        BIGINT,
    bounces       BIGINT,
    page_views    BIGINT,
    requests      BIGINT    NOT NULL,
    errors        BIGINT    NOT NULL,
    not_found     BIGINT    NOT NULL,
    bytes         BIGINT    NOT NULL,
    bot_requests  BIGINT    NOT NULL,
    duration_hist BIGINT[]  NOT NULL,
    PRIMARY KEY (site_id, day)
);

CREATE INDEX IF NOT EXISTS edge_stats_daily_day_idx ON edge_stats_daily (day);

--bun:split

-- One row per (day, dimension, value): the top pages, sources, countries,
-- devices, browsers and systems of a day, so a range read sums rows instead of
-- scanning requests.
CREATE TABLE IF NOT EXISTS edge_stats_daily_top (
    site_id    TEXT   NOT NULL,
    day        DATE   NOT NULL,
    dimension  TEXT   NOT NULL,
    value      TEXT   NOT NULL,
    visitors   BIGINT NOT NULL,
    visits     BIGINT NOT NULL,
    page_views BIGINT NOT NULL,
    PRIMARY KEY (site_id, day, dimension, value)
);

CREATE INDEX IF NOT EXISTS edge_stats_daily_top_day_idx ON edge_stats_daily_top (day);
