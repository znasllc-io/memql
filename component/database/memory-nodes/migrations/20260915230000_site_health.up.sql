-- One current observation per site, not a new site-history version per probe.
CREATE TABLE IF NOT EXISTS site_health (
    site_id TEXT PRIMARY KEY,
    hostname TEXT NOT NULL,
    bundle_ref TEXT NOT NULL,
    checked_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reachable', 'unavailable', 'unknown')),
    reason TEXT NOT NULL DEFAULT '',
    http_status INTEGER NOT NULL DEFAULT 0,
    duration_ms BIGINT NOT NULL DEFAULT 0
);
