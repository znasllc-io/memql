ALTER TABLE email_stat_watermark DROP COLUMN IF EXISTS read_until;

--bun:split

ALTER TABLE email_stat_watermark DROP COLUMN IF EXISTS reread_from;
