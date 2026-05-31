-- Cross-replica automation execution claims (znasllc-io/memql#561).
--
-- When a memQL node-type runs >=2 replicas, the same triggering event can
-- reach more than one replica (the EventBridge dedup is per-process, not
-- cluster-wide). To keep event-triggered automations exactly-once across
-- the cluster, each replica claims an execution by inserting a row keyed by
-- (automation_name, dedup_key); the unique primary key means exactly one
-- replica wins and the others skip. The claim record also IS the audit
-- trail -- claimed_by + claimed_at make any double-fire investigable.
--
-- dedup_key is the automation's InitialChainHead: a deterministic
-- fingerprint of (automation, trigger, event payload, input) -- so the same
-- event produces the same key on every replica.
--
-- Rows are short-lived (a dedup window of minutes); the guard prunes rows
-- older than its retention. The claimed_at index keeps that prune cheap.

--bun:split

CREATE TABLE IF NOT EXISTS automation_execution_claims (
  automation_name text        NOT NULL,
  dedup_key       text        NOT NULL,
  claimed_by      text        NOT NULL,
  claimed_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (automation_name, dedup_key)
);

--bun:split

CREATE INDEX IF NOT EXISTS idx_automation_execution_claims_claimed_at
  ON automation_execution_claims (claimed_at);
