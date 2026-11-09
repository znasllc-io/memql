DELETE FROM email_stat_watermark WHERE through IS NULL;

--bun:split

ALTER TABLE email_stat_watermark ALTER COLUMN through SET NOT NULL;

--bun:split

ALTER TABLE email_stat_watermark DROP COLUMN IF EXISTS walked_through;

--bun:split

DROP TABLE IF EXISTS email_stat_walk_start;
