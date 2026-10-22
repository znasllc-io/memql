ALTER TABLE router_policy_revision
    DROP COLUMN IF EXISTS routes,
    DROP COLUMN IF EXISTS auto;
