-- Native same-deployment continuation. Editable graph rows cannot mint a
-- selection, lease, rollout attempt or publication receipt.
CREATE TABLE IF NOT EXISTS package_deployment_continuations (
    deployment_id text PRIMARY KEY CHECK (length(deployment_id) BETWEEN 1 AND 128),
    owner_user_id text NOT NULL,
    requested_by text NOT NULL,
    package_id text NOT NULL,
    selection_sha256 text NOT NULL CHECK (selection_sha256 ~ '^[0-9a-f]{64}$'),
    body jsonb NOT NULL CHECK (octet_length(body::text) <= 1048576),
    phase text NOT NULL CHECK (phase IN ('prepared','rolling','publishing','completed','refused')),
    state jsonb NOT NULL DEFAULT '{}' CHECK (octet_length(state::text) <= 1048576),
    publications jsonb NOT NULL DEFAULT '{}' CHECK (octet_length(publications::text) <= 1048576),
    domain_receipts jsonb NOT NULL DEFAULT '{}' CHECK (octet_length(domain_receipts::text) <= 1048576),
    cancel_requested boolean NOT NULL DEFAULT false,
    deadline timestamptz NOT NULL,
    lease_id text NOT NULL DEFAULT '',
    lease_until timestamptz NOT NULL DEFAULT 'epoch',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS package_deployment_continuations_pending
 ON package_deployment_continuations (lease_until, deployment_id)
 WHERE phase NOT IN ('completed','refused');
