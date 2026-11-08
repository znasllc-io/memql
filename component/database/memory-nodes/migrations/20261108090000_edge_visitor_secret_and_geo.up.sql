-- The edge's visitor secret and country database.
--
-- edge_visitor_secret: one random 32-byte secret per UTC day, shared by every
-- edge replica so they hash a visitor identically. Rotation deletes earlier
-- days, which is what makes a visitor unlinkable across days. Not a concept:
-- no query reaches it and nothing broadcasts it.
CREATE TABLE IF NOT EXISTS edge_visitor_secret (
    day    DATE  PRIMARY KEY,
    secret BYTEA NOT NULL
);

--bun:split

-- edge_geo_country: the single current country database (an MMDB file), kept
-- in the database so every replica loads the same release without an object
-- store. id is pinned to 1: a newer release replaces the row.
CREATE TABLE IF NOT EXISTS edge_geo_country (
    id         SMALLINT    PRIMARY KEY CHECK (id = 1),
    release    TEXT        NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL,
    data       BYTEA       NOT NULL
);
