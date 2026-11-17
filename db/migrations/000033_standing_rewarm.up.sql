-- docs/adr/0084 §3, slice 5: a standing character is kept up by core.
-- restart_after: when the pod last went down (any terminal path, or a warm that failed). NULL counts as due. The
-- back-off is added at re-warm time from the crash entries of the last hour, so no column counts anything.
-- drain_started_at: a re-pin in place has stopped intake on every thread and waits for the running turn.
ALTER TABLE sessions ADD COLUMN restart_after TIMESTAMPTZ;
ALTER TABLE sessions ADD COLUMN drain_started_at TIMESTAMPTZ;
-- warm_failures: warms that failed since the last one that worked; they back the re-warm off like crash entries.
ALTER TABLE sessions ADD COLUMN warm_failures INT NOT NULL DEFAULT 0;
