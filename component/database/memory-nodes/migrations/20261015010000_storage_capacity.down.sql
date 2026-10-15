DROP TABLE IF EXISTS file_backup_directory_files;
DROP TABLE IF EXISTS file_backup_directories;
DROP TABLE IF EXISTS file_backup_blocks;
DROP TABLE IF EXISTS file_backup_transfers;
DROP TABLE IF EXISTS storage_capacity_blocks;
DROP TABLE IF EXISTS storage_capacity_objects;
DROP TABLE IF EXISTS storage_capacity_users;
DROP TABLE IF EXISTS storage_capacity_accounts;

DROP TABLE IF EXISTS storage_capacity_totals;

DROP FUNCTION IF EXISTS maintain_backup_directory_bytes();
