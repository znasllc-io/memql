-- Role testing covers custom and organization roles. Which role may be tested
-- is decided in Go, against the live role catalog (auth.MayTestRole and the
-- role-test service), so both tables keep only what a role slug always is:
-- non-empty. The constraint names are the ones the original CHECKs carried.
ALTER TABLE identity_role_test_personas
    DROP CONSTRAINT IF EXISTS identity_role_test_personas_role_check;
ALTER TABLE identity_role_test_personas
    ADD CONSTRAINT identity_role_test_personas_role_check CHECK (btrim(role) <> '');
ALTER TABLE identity_role_test_sessions
    DROP CONSTRAINT IF EXISTS identity_role_test_sessions_role_check;
ALTER TABLE identity_role_test_sessions
    ADD CONSTRAINT identity_role_test_sessions_role_check CHECK (btrim(role) <> '');
