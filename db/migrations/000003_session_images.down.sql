ALTER TABLE sessions
  DROP COLUMN IF EXISTS worker_image,
  DROP COLUMN IF EXISTS sidecar_image;
