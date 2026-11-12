-- Drops the account export's record, the record of moved rows, the follow
-- tasks, and the columns this release added to the assignment job's tables.
--
-- It deletes no file from blob storage: an export a person has not removed
-- stays under exports/accounts/ with no record pointing at it. Remove the files
-- first, from the Export tab, when going back for good.
--
-- It moves no row back. A row the governed act moved keeps the account it was
-- given; account_row_moves was the only record of where it came from.
--
-- A follow task that was not done is dropped with its table, so rows that were
-- still to follow a moved parent stay in the account the parent left. The
-- engine of the previous release does not move rows between accounts, so
-- nothing there depends on them having followed.
ALTER TABLE account_assignment_progress DROP COLUMN pass_horizon_at;
ALTER TABLE account_assignment_progress DROP COLUMN full_pass_requested_at;
ALTER TABLE account_assignment_conflict DROP COLUMN left_out_by;
ALTER TABLE account_assignment_conflict DROP COLUMN left_out_at;
ALTER TABLE account_assignment_conflict DROP COLUMN candidates;
DROP TABLE account_follow_tasks;
DROP TABLE account_row_moves;
DROP TABLE account_exports;
