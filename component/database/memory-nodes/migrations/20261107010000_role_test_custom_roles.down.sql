-- Puts back the four-role CHECK. A test of a custom or organization role cannot
-- satisfy it, so down DELETES those tests rather than failing on them:
-- their sessions first (a session references its persona), then the persona
-- rows. Each such test ends, exactly as End would end it. The persona user
-- rows stay as inert identities, as a failed Start leaves one: with no session
-- and no login factor nothing can authenticate as them.
--
-- AN ORGANIZATION ROLE PERSONA'S MEMBERSHIP OUTLIVES THIS, AND NOTHING REMOVES
-- IT BY ITSELF. It is a graph row this SQL does not rewrite, and role testing
-- finds it only through the persona row deleted here (the sweep reads
-- identity_role_test_personas), so no later Start or End reaches it -- not even
-- with the up applied again, whose next test of that role is a new persona. It
-- admits nobody, for the reason above, but the persona stays a member of that
-- organization's group until an operator removes it by hand: groupMemberRemove
-- with the membership's groupId and the persona's userId. After this down every
-- active membership of origin `role-test` is one of these, because only an
-- organization role's test places one; to know them in advance, run before it
--   SELECT role, user_id FROM identity_role_test_personas
--    WHERE role NOT IN ('reader', 'writer', 'developer', 'admin');
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
