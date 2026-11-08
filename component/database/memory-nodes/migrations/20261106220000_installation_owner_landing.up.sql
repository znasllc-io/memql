ALTER TABLE release_installation_proposals ADD COLUMN landed_commit text
 CHECK (landed_commit IS NULL OR (landed_commit ~ '^[0-9a-f]{40}$' AND landed_commit=branch_commit AND pull_request IS NULL AND merged_commit IS NULL));
