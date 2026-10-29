-- docs/adr/0082 §5: MarkSwept now withdraws a swept session's open decisions
-- and clears its unused one-time grants in its own transaction, as Archive
-- already did. Sessions swept before that keep both; this closes them the
-- same way (sessions.withdrawDecisions). A swept session cannot be resumed,
-- so nothing it asked can be acted on.
UPDATE decisions d
   SET state     = CASE WHEN d.state = 'open' THEN 'withdrawn' ELSE d.state END,
       grantable = false,
       version   = nextval('decisions_version_seq')
  FROM sessions s
 WHERE s.id = d.session_id
   AND s.swept_at IS NOT NULL
   AND (d.state = 'open' OR (d.grantable AND d.grant_used_at IS NULL));
