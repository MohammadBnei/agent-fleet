-- Recreates prompt_snippets exactly as 000001_init left it, seed rows included.
CREATE TABLE prompt_snippets (
  id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name                      TEXT NOT NULL UNIQUE,
  text                      TEXT NOT NULL,
  -- NULL means no suggestion. When a session is opened with snippets that
  -- set this, the first non-null suggestion is applied to permission_mode.
  suggested_permission_mode TEXT,
  created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO prompt_snippets (name, text, suggested_permission_mode) VALUES
  ('Architecture interview',
   'If this task involves an architecturally non-trivial decision (new component, schema/protocol/API shape, a one-way door — see the architecture-interview skill''s own DOOR classification), type "/architecture-interview" as your next message to run it before you plan. Otherwise say in one line why it''s skipped and move on.',
   NULL),
  ('Post a plan for approval',
   'Before implementing, post your plan via send_message (from="agent"), citing the specific files/paths you read and relied on. If Mohammad has put this session into plan mode (the dashboard''s mode picker), call ExitPlanMode with that same plan once you''re confident — it blocks for a real human decision the same way it does in Claude Code''s own plan mode. Outside plan mode, just proceed to implementing once you''ve posted the plan.',
   'plan'),
  ('Doubt-driven review',
   'Once you have a plan, if it involves a non-trivial decision per the doubt-driven-development skill''s own "When to Use" checklist, type "/doubt-driven-development" as your next message to run it. Otherwise say why it''s skipped and move on. Its cross-model escalation step is non-interactive in this environment — skip it and announce the skip.',
   NULL),
  ('Open a PR when done',
   'Write the code following your plan, and add or update tests — run the repo''s test suite and make it pass; if there is no test suite, add one first. Update docs if relevant. Then commit your changes with git yourself via Bash, push, and open the PR yourself: create your branch (git checkout -b <name>), push it (git push -u origin <name>), then gh pr create --title "..." --body "..." against this session''s base branch. Your git identity is already configured — just run the commands. Confirm the PR actually exists afterward (e.g. gh pr view) before telling Mohammad it''s ready.',
   NULL);
