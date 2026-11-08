-- The visit fields on the edge's request log: what a deployable's website
-- statistics are counted from (docs/superpowers/specs/
-- 2026-11-08-site-and-email-statistics-design.md, section 4).
--
-- WHAT A ROW NOW CARRIES. Besides a request's outcome: whether it was a page
-- view and whether a bot asked, and for a page view only its path, source,
-- country, device, browser, operating system and a visitor id -- a keyed hash
-- under a secret that exists for one UTC day (edge_visitor_secret). The IP
-- address and the user agent are NOT columns, and must never become ones: the
-- edge derives these fields from them and drops both before a row is written.
-- The values are derived and normalized in component/edge/visit.
--
-- NOT NULL WITH CONSTANT DEFAULTS, for two reasons.
--
--   * The defaults ARE the answer for every row recorded before this
--     release. measured = false says a row was never classified, so a
--     statistic reads such a day as not measured rather than as zero
--     visitors, while its rows keep counting for health. A previous
--     release's edge, still writing during a rolling upgrade, names none of
--     these columns and its rows take the same defaults.
--   * edge_request is a hypertable with compression enabled. TimescaleDB
--     refuses a NOT NULL column without a default, and any non-constant
--     default, on such a table; a constant default is recorded in the
--     catalog, so no chunk, compressed or not, is rewritten. Verified on the
--     CI TimescaleDB image (2.29) with a compressed chunk present.
--
-- NO INDEX. Every read of these columns is a rollup over one site's hours or
-- days, which the existing (site_id, served_at) index already serves.
ALTER TABLE "edge_request"
    ADD COLUMN IF NOT EXISTS measured  BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS page_view BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS bot       BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS path      TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS country   TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS device    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS browser   TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS os        TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS visitor   TEXT    NOT NULL DEFAULT '';
