-- Drops the organization a role test ran in. Every session is kept, and End
-- records its role's current organization again, as it did before the column --
-- for a test that ran in another organization too.
ALTER TABLE identity_role_test_sessions DROP COLUMN IF EXISTS account_id;
