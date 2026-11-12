-- The store sales facts: what the store statistics are computed from.
--
-- SIDE TABLES, NOT GRAPH ROWS. None of the seven is a concept and none is in
-- the row table. They are keyed by store, and a fact's account is its store's,
-- reached through store_id; no column holds an account, because a copy can lag
-- its source. A fact is derived, and can be rebuilt from the mirror rows it is
-- computed from.
--
-- Every id is BARE: store_id, order_gid, line_gid and refund_gid are the short
-- slugs the mirror rows carry, never a prefixed form.
--
-- NO PERSONAL DATA. A buyer is held as buyer_key, a key that stands for the
-- buyer and names no one; no name, address, email or phone is kept.
--
-- A VERSION is the mirror row's own write time (order_version, line_version,
-- refund_version). A write of an older version than the one held is refused, so
-- a replayed or reordered page never moves a fact backwards.
--
-- A RETIRED ROW'S FACT IS KEPT. When the mirror retires a row the fact stays,
-- marked deleted: it counts nowhere, and it keeps refusing every older version,
-- so a stale page cannot bring the row back.
--
-- Money is NUMERIC, so sums are exact. Statements are separated by the bun
-- split marker, and every statement may run again.

CREATE TABLE IF NOT EXISTS store_sales_order_fact (
    store_id                 TEXT NOT NULL,
    order_gid                TEXT NOT NULL,
    order_version            TIMESTAMPTZ NOT NULL,            -- the mirror row's own write time (row.createdAt)
    deleted                  BOOLEAN NOT NULL DEFAULT false,  -- the mirror retired the row: the fact stays, counts nowhere and refuses every older version
    recorded_at              TIMESTAMPTZ NOT NULL,            -- the row's syncedAt
    ordered_at               TIMESTAMPTZ NOT NULL,
    currency                 TEXT NOT NULL,
    cancelled                BOOLEAN NOT NULL,
    test                     BOOLEAN NOT NULL,
    taxes_included           BOOLEAN NOT NULL,
    buyer_key                TEXT NOT NULL DEFAULT '',
    wholesale                BOOLEAN NOT NULL DEFAULT false,
    shipping                 NUMERIC NOT NULL,
    tax                      NUMERIC NOT NULL,
    duties                   NUMERIC NOT NULL,
    expected_quantity        INTEGER NOT NULL,
    shopify_current_subtotal NUMERIC,
    shopify_total_discounts  NUMERIC,
    shopify_total_refunded   NUMERIC,
    gross                    NUMERIC NOT NULL DEFAULT 0,      -- derived
    discounts                NUMERIC NOT NULL DEFAULT 0,      -- derived
    units                    INTEGER NOT NULL DEFAULT 0,      -- derived
    line_count               INTEGER NOT NULL DEFAULT 0,      -- derived
    lines_version            TIMESTAMPTZ,                     -- derived
    lines_complete           BOOLEAN NOT NULL DEFAULT false,  -- derived
    checked_at               TIMESTAMPTZ,
    PRIMARY KEY (store_id, order_gid)
);
-- read by: the orders of a period (the read's figures and series)
CREATE INDEX IF NOT EXISTS store_sales_order_fact_ordered_idx ON store_sales_order_fact (store_id, ordered_at);
-- read by: a buyer's first counted order, for new against returning
CREATE INDEX IF NOT EXISTS store_sales_order_fact_buyer_idx ON store_sales_order_fact (store_id, buyer_key, ordered_at);

--bun:split

CREATE TABLE IF NOT EXISTS store_sales_line_fact (
    store_id            TEXT NOT NULL,
    line_gid            TEXT NOT NULL,
    order_gid           TEXT NOT NULL DEFAULT '',        -- '' only on a deleted fact whose tombstone carried no parent
    line_version        TIMESTAMPTZ NOT NULL,
    deleted             BOOLEAN NOT NULL DEFAULT false,
    currency            TEXT NOT NULL DEFAULT '',        -- of its unit price: it picks the scale a share is rounded at
    gift_card           BOOLEAN NOT NULL,
    product_key         TEXT NOT NULL,
    title               TEXT NOT NULL,
    quantity            INTEGER NOT NULL,
    current_quantity    INTEGER NOT NULL,
    unit_price          NUMERIC NOT NULL,
    allocated_discounts NUMERIC NOT NULL,
    refunded_quantity   INTEGER NOT NULL DEFAULT 0,  -- derived
    sold_quantity       INTEGER NOT NULL DEFAULT 0,  -- derived
    gross               NUMERIC NOT NULL DEFAULT 0,  -- derived
    discounts           NUMERIC NOT NULL DEFAULT 0,  -- derived
    checked_at          TIMESTAMPTZ,
    PRIMARY KEY (store_id, line_gid)
);
-- read by: the lines of an order, in the derivation and in every figure that reaches a line through its order
CREATE INDEX IF NOT EXISTS store_sales_line_fact_order_idx ON store_sales_line_fact (store_id, order_gid);

--bun:split

CREATE TABLE IF NOT EXISTS store_sales_refund_fact (
    store_id           TEXT NOT NULL,
    refund_gid         TEXT NOT NULL,
    order_gid          TEXT NOT NULL DEFAULT '',
    refund_version     TIMESTAMPTZ NOT NULL,
    deleted            BOOLEAN NOT NULL DEFAULT false,
    recorded_at        TIMESTAMPTZ NOT NULL,
    refunded_at        TIMESTAMPTZ NOT NULL,
    money_refunded     NUMERIC NOT NULL,
    shipping_refunded  NUMERIC,              -- null while the detail is not read
    tax_refunded       NUMERIC,              -- as Shopify wrote it; a figure reads it as 0 for an order whose prices include tax
    duties_refunded    NUMERIC,
    detail_fetched     BOOLEAN NOT NULL,
    lines_complete     BOOLEAN NOT NULL,
    gift_card_refunded NUMERIC,              -- derived
    item_refunds       NUMERIC,              -- derived
    units_returned     INTEGER,              -- derived
    checked_at         TIMESTAMPTZ,
    PRIMARY KEY (store_id, refund_gid)
);
-- read by: the refunds of a period (the read's figures and series)
CREATE INDEX IF NOT EXISTS store_sales_refund_fact_refunded_idx ON store_sales_refund_fact (store_id, refunded_at);
-- read by: the refunds of an order, in the derivation
CREATE INDEX IF NOT EXISTS store_sales_refund_fact_order_idx ON store_sales_refund_fact (store_id, order_gid);

--bun:split

CREATE TABLE IF NOT EXISTS store_sales_refund_line_fact (
    store_id   TEXT NOT NULL,
    refund_gid TEXT NOT NULL,
    position   INTEGER NOT NULL,
    order_gid  TEXT NOT NULL,
    line_gid   TEXT NOT NULL,
    quantity   INTEGER NOT NULL,
    subtotal   NUMERIC NOT NULL,
    tax        NUMERIC NOT NULL,
    PRIMARY KEY (store_id, refund_gid, position)
);
-- read by: the units refunded of each line of an order, in the derivation
CREATE INDEX IF NOT EXISTS store_sales_refund_line_fact_line_idx ON store_sales_refund_line_fact (store_id, order_gid, line_gid);

--bun:split

CREATE TABLE IF NOT EXISTS store_sales_store (
    store_id                 TEXT PRIMARY KEY,
    generation               BIGINT NOT NULL DEFAULT 0,       -- raised by a rebuild and by a removal: a page read under an older one writes nothing
    purging                  BOOLEAN NOT NULL DEFAULT false,  -- the store's facts are being deleted in batches; nothing is folded meanwhile
    store_domain             TEXT NOT NULL DEFAULT '',
    store_name               TEXT NOT NULL DEFAULT '',
    currency                 TEXT NOT NULL DEFAULT '',
    timezone                 TEXT NOT NULL DEFAULT '',
    taxes_included           BOOLEAN,
    development              BOOLEAN,
    all_orders_readable      BOOLEAN NOT NULL DEFAULT false,
    history_floor            TIMESTAMPTZ,                     -- an instant before the store row was first written: where every walk of the store begins
    history_since            TIMESTAMPTZ,                     -- the store-local midnight of the first recorded day
    wholesale_enabled        BOOLEAN NOT NULL DEFAULT false,
    wholesale_digest         TEXT NOT NULL DEFAULT '',
    wholesale_cursor         TEXT NOT NULL DEFAULT '',
    wholesale_window_seconds INTEGER NOT NULL DEFAULT 86400,
    verdict_state            TEXT NOT NULL DEFAULT '',
    verdict_reasons          JSONB NOT NULL DEFAULT '[]',
    verdict_since            TIMESTAMPTZ,                     -- the verdict's own since, as last read
    recording_blocked        BOOLEAN NOT NULL DEFAULT false,  -- at the last verdict read, something stood in the way of the walked concepts, or the store was paused or not connected
    last_clear_at            TIMESTAMPTZ,                     -- the last run at which nothing stood in the way: what the figures are as of while recording_blocked
    last_change_at           TIMESTAMPTZ,
    walked_through           TIMESTAMPTZ,
    rebuilding_since         TIMESTAMPTZ,
    definition               INTEGER NOT NULL DEFAULT 0
);

--bun:split

CREATE TABLE IF NOT EXISTS store_sales_watermark (
    store_id       TEXT NOT NULL,
    source         TEXT NOT NULL,                    -- order, line or refund
    through        TIMESTAMPTZ NOT NULL,
    window_seconds INTEGER NOT NULL DEFAULT 86400,
    walked_through TIMESTAMPTZ,
    PRIMARY KEY (store_id, source)
);

--bun:split

CREATE TABLE IF NOT EXISTS store_sales_check (
    store_id              TEXT PRIMARY KEY,
    -- the pass under way
    pass_started_at       TIMESTAMPTZ,                     -- null when none is under way
    order_cursor          TEXT NOT NULL DEFAULT '',        -- '', an instant to go on from, or 'done'
    order_window_seconds  INTEGER NOT NULL DEFAULT 86400,
    line_cursor           TEXT NOT NULL DEFAULT '',
    line_window_seconds   INTEGER NOT NULL DEFAULT 86400,
    refund_cursor         TEXT NOT NULL DEFAULT '',
    refund_window_seconds INTEGER NOT NULL DEFAULT 86400,
    compared              INTEGER NOT NULL DEFAULT 0,
    mismatched            INTEGER NOT NULL DEFAULT 0,
    repaired              INTEGER NOT NULL DEFAULT 0,
    -- the last pass that ended
    last_started_at       TIMESTAMPTZ,
    last_finished_at      TIMESTAMPTZ,
    last_compared         INTEGER NOT NULL DEFAULT 0,
    last_mismatched       INTEGER NOT NULL DEFAULT 0,
    last_repaired         INTEGER NOT NULL DEFAULT 0,
    last_disagreeing      INTEGER NOT NULL DEFAULT 0,
    clean_at              TIMESTAMPTZ                      -- when a pass last ended with nothing mismatched
);
