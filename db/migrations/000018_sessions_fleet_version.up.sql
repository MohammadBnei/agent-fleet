-- The release that built a session's pod, recorded next to worker_image and
-- sidecar_image from CreateWorkerPodResponse.fleet_version. Images are tagged
-- src-<input hash> since agent-fleet#300, so the image refs no longer name a
-- release. NOT NULL DEFAULT '' for the same reason as 000003: it scans into a
-- plain Go string. '' = a pod from before the provisioner reported it.
ALTER TABLE sessions ADD COLUMN fleet_version TEXT NOT NULL DEFAULT '';
