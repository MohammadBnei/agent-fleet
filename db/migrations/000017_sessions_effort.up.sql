-- The effort level a repo session launches with, picked in the New Session
-- dialog. NULL = none chosen: the worker passes neither effort nor thinking,
-- and Claude Code decides. A definition session leaves it NULL and takes its
-- spec's thinking.default instead. Validated in core against the SDK's
-- EffortLevel (low|medium|high|xhigh|max), not here.
ALTER TABLE sessions ADD COLUMN effort TEXT;
