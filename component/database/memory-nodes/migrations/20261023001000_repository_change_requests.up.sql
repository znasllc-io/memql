-- Private authority journal. All access is through owner predicates and native
-- request/effect gates; it is not an editable MemoryNodes concept.
CREATE TABLE repository_change_requests (
 id text PRIMARY KEY,
 requester text NOT NULL,
 maintainer text NOT NULL,
 source_id text NOT NULL,
 fingerprint text NOT NULL,
 body jsonb NOT NULL,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX repository_change_requester_idx ON repository_change_requests(requester,source_id,created_at DESC);
CREATE INDEX repository_change_maintainer_idx ON repository_change_requests(maintainer,source_id,created_at DESC);
