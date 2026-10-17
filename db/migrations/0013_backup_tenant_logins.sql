-- Per-service backup logins. The tenant restic REST server runs --private-repos --append-only, so a host
-- can only reach its own repository (/<repo_user>) and cannot delete or rewrite snapshots. Retention runs
-- from the control plane through a separate admin endpoint. The login password is sealed with the KEK.
ALTER TABLE service_backup_config
  ADD COLUMN repo_user text UNIQUE CHECK (repo_user ~ '^s-[0-9a-f-]{36}$'),
  ADD COLUMN repo_password_sealed bytea,
  ADD CONSTRAINT backup_repo_login_complete CHECK ((repo_user IS NULL) = (repo_password_sealed IS NULL));
