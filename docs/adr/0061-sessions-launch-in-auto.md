# ADR-0061 — Sessions launch in `auto`, on Opus 5.5

- **Status:** Accepted — definition sessions are an exception to the `auto` default: [ADR-0062](0062-defined-agents.md) writes their mode explicitly and forbids `auto`; amended by [ADR-0076](0076-definition-sessions-run-in-auto.md): definitions now launch in the fleet's `auto` too
- **Date:** 2026-10-16
- **Amends:** [ADR-0029](0029-sessions-not-tasks-permission-prompt-not-approval-gate.md)
  (sessions start in `"default"` for CLI parity),
  [ADR-0052](0052-auto-mode-and-the-bypass-launch-profile.md) (names the model
  the fleet runs), [ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md)
  (`auto` as an opt-in a human picks)
- **Amended 2026-10-20:** guidance snippets were removed (migration 000013,
  replaced by context packs, [ADR-0063](0063-context-packs.md)). The
  "snippet's suggested mode" below no longer exists: a session launches in
  `auto` unless a human picks another mode, and plan mode is now only that
  explicit pick.
- **Amended 2026-10-21:** §4 is reversed by
  [ADR-0077](0077-claude-code-picks-the-model.md). The fleet pins no model:
  with no explicit pick and no `CLAUDE_MODEL`, the row stores NULL, the worker
  passes no model, and Claude Code picks. The `||` in the worker stays.

## Context

Until now every session launched in the SDK's `"default"` mode unless a human
picked another one in the new-session dialog, or a snippet suggested one. Since
[ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md), `auto` means the
worker answers every prompt itself except `Bash` running `rm` or `sudo`. The
owner asked for `auto` to be the default.

Separately, Agent SDK 0.3.301 (Claude Code 2.1.301) supports `claude-opus-5-5`.
The fleet ran `claude-opus-5`.

## Decision

1. **`auto` is the launch default for every new session.** The
   `sessions.permission_mode` column default becomes `'auto'`
   (`db/migrations/000007`), and `sessions.Store.Create` uses the same value.
   This applies to every path that creates a session, including sessions opened
   from proposals (alert, audit and schedule). The new-session dialog
   pre-selects `auto` and still shows `AUTO_MODE_WARNING`. A snippet's suggested
   mode and an explicit pick still win. Existing rows keep their mode.
2. **The worker's own fallbacks stay `"default"`.** A worker that gets a
   session row with no mode (a missing field, version skew between pods) must
   fail closed, not launch unattended. The default lives in core only.
3. **A refused launch is detected.** `auto` has its own gates in the CLI (a
   model allow-list and a remote circuit breaker), and the SDK can refuse it at
   boot just as it refuses a mid-session switch. The worker now compares the
   `init` message's `permissionMode` with the mode it launched in. If they
   differ, the gate follows the SDK and the column is corrected. Before this,
   the gate would have kept answering as `auto` while the SDK was still asking.
4. **The default model is `claude-opus-5-5`.** `OpenFromProposal` now writes
   the model too. Before, it stored NULL and the worker fell back to
   `CLAUDE_MODEL`, which the provisioner always sets, empty when core has none.
   `??` kept the empty string, so proposal sessions ran on the CLI's built-in
   default. The worker now uses `||`.

## Consequences

- **Proposal bodies run unattended.** A proposal-opened session sends the
  proposal body as its first message. That text comes from Alertmanager
  payloads and scheduled audits, not from a human. Under `auto`, a body that
  carries an instruction can run `git push`, `gh`, `kubectl` through the
  executor, `curl` or `env` with no prompt. This is a prompt-injection path,
  and it was accepted knowingly. A human still opens each proposal; nothing
  opens one on its own.
- **`env` output reaches the transcript by default.** `AUTO_MODE_WARNING`
  already says an unattended `env` puts `GH_TOKEN` in a rendered transcript.
  That exposure used to be opt-in. Now it is the default.
- **The `auto` badge stops being a signal.** The list colours `auto` as a
  warning. When almost every session is `auto`, the badge marks the normal
  case, not the exception.
- **Plan mode is no longer where a session starts.** `ExitPlanMode` still asks
  in `auto`, but nothing enters plan mode unless a human or the "Post a plan
  for approval" snippet picks it.
- **"Every result is a PR" still holds, but no human approves the push.** The
  `git push` ask rule from [ADR-0049](0049-project-setting-source.md) is still
  injected, and in `auto` the worker answers it.
- If the CLI refuses `auto` for `claude-opus-5-5` (or a later CLI drops it from
  the allow-list), point 3 makes that show up as a corrected badge and a
  warning log, not as a gate that silently answers for a human.
