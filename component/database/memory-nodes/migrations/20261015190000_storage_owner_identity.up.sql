-- The capacity inventory originally charged canonical Library owners separately
-- from the bare IDs used by admission and the usage read. Merge both spellings
-- without moving objects or releasing any used/reserved bytes.
LOCK TABLE storage_capacity_objects, storage_capacity_totals, storage_capacity_users IN ACCESS EXCLUSIVE MODE;

INSERT INTO storage_capacity_totals(scope, used_bytes, reserved_bytes)
SELECT 'user:' || substr(scope, length('user:v1:identity:user:') + 1), used_bytes, reserved_bytes
FROM storage_capacity_totals
WHERE scope LIKE 'user:v1:identity:user:%'
ON CONFLICT(scope) DO UPDATE SET
    used_bytes = storage_capacity_totals.used_bytes + EXCLUDED.used_bytes,
    reserved_bytes = storage_capacity_totals.reserved_bytes + EXCLUDED.reserved_bytes;
DELETE FROM storage_capacity_totals WHERE scope LIKE 'user:v1:identity:user:%';

UPDATE storage_capacity_objects
SET owner_id = substr(owner_id, length('v1:identity:user:') + 1)
WHERE owner_id LIKE 'v1:identity:user:%';

-- Preserve the most recently assigned allowance if both keys existed.
INSERT INTO storage_capacity_users(owner_id, limit_bytes, changed_by, updated_at)
SELECT substr(owner_id, length('v1:identity:user:') + 1), limit_bytes, changed_by, updated_at
FROM storage_capacity_users WHERE owner_id LIKE 'v1:identity:user:%'
ON CONFLICT(owner_id) DO UPDATE SET
    limit_bytes = EXCLUDED.limit_bytes, changed_by = EXCLUDED.changed_by, updated_at = EXCLUDED.updated_at
WHERE EXCLUDED.updated_at > storage_capacity_users.updated_at;
DELETE FROM storage_capacity_users WHERE owner_id LIKE 'v1:identity:user:%';
