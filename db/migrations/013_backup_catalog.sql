ALTER TABLE backups ALTER COLUMN environment_id DROP NOT NULL;
CREATE UNIQUE INDEX backup_repositories_native_identity ON backup_repositories(native_id);
