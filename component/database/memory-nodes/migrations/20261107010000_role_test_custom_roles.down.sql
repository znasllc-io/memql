-- Puts back the four-role CHECK. A test of a custom or organization role cannot
-- satisfy it, so down DELETES those tests rather than failing on them:
-- their sessions first (a session references its persona), then the persona
-- rows. Each such test ends, exactly as End would end it. The persona user
-- rows stay as inert identities, as a failed Start leaves one: with no session
-- and no login factor nothing can authenticate as them. An organization
-- persona's role-test membership is a graph row this SQL does not rewrite; it
-- admits nobody for the same reason, and the up's code removes it on that
-- operator's next Start or End if the up is applied again.
DELETE FROM identity_role_test_sessions
    WHERE role NOT IN ('reader', 'writer', 'developer', 'admin')
       OR user_id IN (SELECT user_id FROM identity_role_test_personas WHERE role NOT IN ('reader', 'writer', 'developer', 'admin'));
DELETE FROM identity_role_test_personas
    WHERE role NOT IN ('reader', 'writer', 'developer', 'admin');
ALTER TABLE identity_role_test_sessions
    DROP CONSTRAINT IF EXISTS identity_role_test_sessions_role_check;
ALTER TABLE identity_role_test_sessions
    ADD CONSTRAINT identity_role_test_sessions_role_check CHECK (role IN ('reader', 'writer', 'developer', 'admin'));
ALTER TABLE identity_role_test_personas
    DROP CONSTRAINT IF EXISTS identity_role_test_personas_role_check;
ALTER TABLE identity_role_test_personas
    ADD CONSTRAINT identity_role_test_personas_role_check CHECK (role IN ('reader', 'writer', 'developer', 'admin'));
