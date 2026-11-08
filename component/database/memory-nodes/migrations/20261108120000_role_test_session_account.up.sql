-- The organization a role test ran in, so End records that one rather than
-- wherever its role points by then (component/identity/roletest). Start writes
-- it on the session it issues: an organization role's organization, or '' for
-- a global role. NULL is a session written before this column, and End falls
-- back to the role's current organization for it, as every End did until now.
-- Nullable with no default, so the two stay apart.
ALTER TABLE identity_role_test_sessions ADD COLUMN IF NOT EXISTS account_id text;
