-- Capacity is owned by physical objects, independently of editable Library rows.
-- Reservations survive unknown provider outcomes and never expire on a timer.
CREATE TABLE storage_capacity_accounts (
    account text PRIMARY KEY,
    inventoried boolean NOT NULL DEFAULT false,
    blocked_reason text NOT NULL DEFAULT ''
);
CREATE TABLE storage_capacity_totals (
    scope text PRIMARY KEY,
    used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0)
);
CREATE TABLE storage_capacity_users (
    owner_id text PRIMARY KEY,
    limit_bytes bigint NOT NULL CHECK (limit_bytes BETWEEN 0 AND 5000000000000),
    changed_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE storage_capacity_objects (
    object_key text PRIMARY KEY,
    account text NOT NULL REFERENCES storage_capacity_accounts(account),
    owner_id text NOT NULL,
    used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    capacity_bytes bigint NOT NULL CHECK (capacity_bytes >= used_bytes),
    planned_bytes bigint NOT NULL DEFAULT 0 CHECK (planned_bytes >= 0),
    retired boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX storage_capacity_objects_owner ON storage_capacity_objects(owner_id);
CREATE TABLE storage_capacity_blocks (
    object_key text NOT NULL REFERENCES storage_capacity_objects(object_key),
    block_id text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes > 0),
    committed boolean NOT NULL DEFAULT false,
    PRIMARY KEY (object_key, block_id, committed)
);
CREATE TABLE file_backup_transfers (
    transfer_id text PRIMARY KEY,
    owner_id text NOT NULL,
    worker_id text NOT NULL,
    source_path text NOT NULL,
    source_kind text NOT NULL CHECK (source_kind IN ('archive', 'sync_file', 'download')),
    name text NOT NULL,
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 0 AND 1000000000000),
    source_sha256 text NOT NULL DEFAULT '',
    object_key text NOT NULL,
    state text NOT NULL,
    desired_state text NOT NULL DEFAULT 'running',
    lease_id text NOT NULL,
    next_lease_id text NOT NULL DEFAULT '',
    committed_bytes bigint NOT NULL DEFAULT 0,
    etag text NOT NULL DEFAULT '',
    metadata jsonb NOT NULL DEFAULT '{}',
    failure text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX file_backup_transfers_owner ON file_backup_transfers(owner_id, created_at DESC, transfer_id DESC);
CREATE TABLE file_backup_blocks (
    transfer_id text NOT NULL REFERENCES file_backup_transfers(transfer_id),
    ordinal integer NOT NULL CHECK (ordinal BETWEEN 0 AND 49999),
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 67108864),
    md5 text NOT NULL,
    sha256 text NOT NULL,
    PRIMARY KEY (transfer_id, ordinal)
);
-- The logical live folder ceiling is separate from physical/Bin accounting.
CREATE TABLE file_backup_directories (
    watch_id text PRIMARY KEY,
    owner_id text NOT NULL,
    worker_id text NOT NULL,
    root_path text NOT NULL,
    logical_bytes bigint NOT NULL DEFAULT 0 CHECK (logical_bytes BETWEEN 0 AND 1000000000000)
);
CREATE TABLE file_backup_directory_files (
    watch_id text NOT NULL REFERENCES file_backup_directories(watch_id),
    path text NOT NULL,
    file_id text NOT NULL DEFAULT '',
    live_bytes bigint NOT NULL DEFAULT 0 CHECK (live_bytes >= 0),
    pending_id text NOT NULL DEFAULT '',
    pending_bytes bigint NOT NULL DEFAULT 0 CHECK (pending_bytes >= 0),
    PRIMARY KEY(watch_id,path)
);

-- Keep admission O(1) as the number of files grows. Every mutation of a path
-- changes its parent's counter in the same transaction, including Bin receipts.
CREATE FUNCTION maintain_backup_directory_bytes() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE previous_bytes bigint := 0; next_bytes bigint := 0;
BEGIN
    IF TG_OP <> 'INSERT' THEN previous_bytes := greatest(OLD.live_bytes, OLD.pending_bytes); END IF;
    IF TG_OP <> 'DELETE' THEN next_bytes := greatest(NEW.live_bytes, NEW.pending_bytes); END IF;
    UPDATE file_backup_directories SET logical_bytes = logical_bytes + next_bytes - previous_bytes
    WHERE watch_id = CASE WHEN TG_OP = 'DELETE' THEN OLD.watch_id ELSE NEW.watch_id END;
    RETURN NULL;
END;
$$;
CREATE TRIGGER file_backup_directory_bytes AFTER INSERT OR UPDATE OR DELETE ON file_backup_directory_files
FOR EACH ROW EXECUTE FUNCTION maintain_backup_directory_bytes();
