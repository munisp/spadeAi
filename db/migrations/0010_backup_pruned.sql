-- Backups whose restic snapshot was removed by retention (forget --prune) can no longer be restored.
ALTER TABLE backup_snapshot DROP CONSTRAINT backup_snapshot_status_check;
ALTER TABLE backup_snapshot ADD CONSTRAINT backup_snapshot_status_check
  CHECK (status IN ('running','succeeded','failed','pruned'));
