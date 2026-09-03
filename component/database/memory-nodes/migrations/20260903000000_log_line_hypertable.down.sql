-- Reverse the log store. The compression policy and the indexes go with the
-- table; there is no continuous aggregate and no retention policy to drop.
DROP TABLE IF EXISTS "log_line";
