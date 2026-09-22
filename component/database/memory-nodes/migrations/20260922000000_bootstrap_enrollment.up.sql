-- Enrollment authority is not an identity session. Keep unfinished claims
-- durable across replicas and persist verified attestation before graph writes.
CREATE TABLE IF NOT EXISTS identity_bootstrap_enrollment (
    token_hash text PRIMARY KEY,
    payload jsonb NOT NULL,
    reserved boolean NOT NULL DEFAULT false,
    expires_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS identity_bootstrap_single_owner
    ON identity_bootstrap_enrollment (reserved) WHERE reserved;
