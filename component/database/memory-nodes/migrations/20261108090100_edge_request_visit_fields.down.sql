-- Drops the visit fields from the edge's request log. This discards the visit
-- data of every raw request row still retained; the rows themselves stay and
-- keep counting for health, exactly as they did before the fields existed.
ALTER TABLE "edge_request"
    DROP COLUMN IF EXISTS visitor,
    DROP COLUMN IF EXISTS os,
    DROP COLUMN IF EXISTS browser,
    DROP COLUMN IF EXISTS device,
    DROP COLUMN IF EXISTS country,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS path,
    DROP COLUMN IF EXISTS bot,
    DROP COLUMN IF EXISTS page_view,
    DROP COLUMN IF EXISTS measured;
