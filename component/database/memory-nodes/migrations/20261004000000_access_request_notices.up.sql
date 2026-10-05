-- One durable throttle claim per operator address, shared by identity replicas.
CREATE TABLE IF NOT EXISTS memql_access_request_notices (
    recipient text PRIMARY KEY,
    claimed_at timestamptz NOT NULL
);
