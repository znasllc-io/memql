-- Reverse 20260531120000_automation_execution_claims (#561).
-- NOTE: no leading `--bun:split` (a comment-only segment makes bun
-- emit "query is empty"). See the .up.sql header + memql#570.

DROP TABLE IF EXISTS automation_execution_claims;
