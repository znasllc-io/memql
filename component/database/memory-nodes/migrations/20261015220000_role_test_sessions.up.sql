-- Authentication material stays outside the graph/query surface. Only hashes
-- are stored; personas themselves are ordinary identity:user rows.
CREATE TABLE identity_role_test_personas (
    operator_id text NOT NULL,
    role text NOT NULL CHECK (role IN ('reader', 'writer', 'developer', 'admin')),
    user_id text NOT NULL UNIQUE,
    PRIMARY KEY (operator_id, role)
);
CREATE TABLE identity_role_test_sessions (
    id text PRIMARY KEY,
    token_hash text NOT NULL UNIQUE,
    operator_id text NOT NULL,
    operator_epoch bigint NOT NULL,
    user_id text NOT NULL REFERENCES identity_role_test_personas(user_id),
    role text NOT NULL CHECK (role IN ('reader', 'writer', 'developer', 'admin')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX identity_role_test_sessions_operator ON identity_role_test_sessions(operator_id);
