DROP TABLE IF EXISTS edge_stats_month_day;

--bun:split

DROP TABLE IF EXISTS edge_stats_monthly_top;

--bun:split

DROP TABLE IF EXISTS edge_stats_window_top;

--bun:split

ALTER TABLE edge_stats_daily DROP COLUMN IF EXISTS last_hour_visitors;

--bun:split

ALTER TABLE edge_stats_daily DROP COLUMN IF EXISTS first_hour_visitors;

--bun:split

DROP TABLE IF EXISTS edge_stats_value_hour;

--bun:split

DROP TABLE IF EXISTS edge_stats_visitor;

--bun:split

DROP TABLE IF EXISTS edge_stats_settled_hour;

--bun:split

DROP TABLE IF EXISTS edge_stats_progress;
