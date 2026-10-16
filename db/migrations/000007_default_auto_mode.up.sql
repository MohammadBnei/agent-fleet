-- New sessions launch in `auto` (docs/adr/0061). Rows already stored keep
-- whatever mode they have: this only changes what an INSERT that omits the
-- column gets. core's sessions.Store.Create names 'auto' itself too, so the
-- two cannot disagree for sessions core creates.
ALTER TABLE sessions ALTER COLUMN permission_mode SET DEFAULT 'auto';
