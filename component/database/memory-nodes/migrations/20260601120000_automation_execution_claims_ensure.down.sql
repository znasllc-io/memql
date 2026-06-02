-- Down for the claims ensure migration (memql#624).
--
-- The automation_execution_claims table's lifecycle belongs to
-- 20260531120000_automation_execution_claims; this ensure migration only
-- force-creates it when a poisoned migration ledger left it
-- recorded-applied-but-empty. Reversing this migration must therefore NOT
-- drop the table -- that would violate the original migration's invariant
-- (it believes the table exists). This down is a deliberate no-op.
--
-- NOTE: a statement is present so bun does not emit an empty query for a
-- comment-only segment and abort (memql#570).

DO $$ BEGIN PERFORM 1; END $$;
