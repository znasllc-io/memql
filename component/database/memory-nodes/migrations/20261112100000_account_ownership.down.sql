-- Drops account stamping's switch and the assignment job's state.
--
-- It takes accountId off no row. A row stamped while the switch was on keeps
-- the field, and an engine older than the release that added it refuses to
-- update a row carrying a key its concept does not declare. So this is safe on
-- its own only while the switch is still off; once it has turned on, go back
-- only to engines that declare the field, or restore a backup taken before it
-- did.
--
-- An engine of this release that is still running logs a failed pass each
-- minute until it is replaced (AccountOwnershipRunner.run), and goes on
-- stamping if it had already seen the switch on, so roll the engines back with
-- it.
--
-- Applying the up migration again starts with the switch off and no progress:
-- a fleet that all advertises turns it on at the next check, and the job walks
-- every concept from the start.
DROP TABLE account_assignment_conflict;
DROP TABLE account_assignment_progress;
DROP TABLE account_stamping_readers;
DROP TABLE account_stamping_storage;
