# ADR-0083: Opening fleet sessions in claude.ai

- **Status:** Proposed. No part is built. Step 2 is gated on a spike.
- **Date:** 2026-11-06
- **Related:** [`docs/claude-ai-integration-feasibility.md`](../claude-ai-integration-feasibility.md) (the study: evidence, options, unknowns U1–U6),
  [ADR-0060](0060-claude-codes-own-cross-session-channel-is-open.md) (§3 `remoteControlAtStartup: false`, which step 2 would amend),
  [ADR-0064](0064-definition-pod-profile.md) (definition pods close Claude Code's own channels),
  [ADR-0068](0068-islands.md) (no cross-site iframe),
  [ADR-0057](0057-coreservice-authenticates-with-the-session-lease.md) (per-method lease table)

## Context

The owner asked whether the console can open Claude Code sessions hosted on claude.ai (claude.ai/code and the mobile app) directly, ideally embedded in frames. The study found three things.

- **claude.ai refuses framing.** It sends `X-Frame-Options: SAMEORIGIN` and `COOP: same-origin`, and its login cookie would be third-party inside `fleet.bnei.dev`.
- **There are two workable directions:**
  - **A.** Launch a claude.ai cloud session (a routine fire returns `claude_code_session_url`). The fleet then sees nothing of it: no public API reads its transcript, and decisions, vault and slot cap are all bypassed.
  - **B.** Turn on Remote Control in a fleet pod, so the pod's session also appears in claude.ai. The fleet keeps everything, but four things are unverified: whether it runs under the Agent SDK headless, whether the fleet's token can register, how a phone answer meets `canUseTool`, and whether phone input reaches the transcript.
- **Self-hosted environments (C) are Team/Enterprise only.**

## Decision (proposed)

1. **No iframe.** A claude.ai session is opened as a link (`target="_blank" rel="noreferrer"`) from the SESSION facts in `SessionPanels.tsx`. Core's `frame-ancestors 'none'` stays as is.
2. **Remote Control, opt-in, repo sessions only, after a spike.**
   - **The spike:** a `/kind-local` run answering U1–U6 of the study. If the fleet's `CLAUDE_CODE_OAUTH_TOKEN` cannot register (U3), stop there and bring the credential question back to the owner.
   - **If the spike passes:**
     - a per-session opt-in sets `remoteControlAtStartup: true` for that session's worker;
     - definition pods never get it;
     - the worker reports the Remote Control URL through the sidecar on a new lease-authenticated `CoreService` method;
     - this ADR is amended with the spike's answer to U4 (is a phone answer a recorded decision, accepted, or fenced?).
3. **One storage field for both directions.** It is a new nullable URL on `sessions` and `Session`, under a new name and field number (`kind`, 15 and 16 stay reserved). It is shown as a link and **never written to git**: `check-trailers.py` refuses claude.ai URLs in commits and PR text.
4. **Launching on claude.ai (A) is not a fleet backend.** It may ship later as a link-out only: a row marked external, with no pod, lifecycle or transcript. It has its own ADR if wanted.

## Consequences

- **A second decision channel is possible (B, U4).** It is the same class of gap ADR-0060 accepted for `SendMessage`, but for permissions. This ADR does not accept it in advance.
- **Account-wide reach, as in ADR-0060.** Anyone signed in to the account can drive an opted-in pod from claude.ai or the phone. On a personal plan that is the owner.
- **Re-check on every CLI bump.** `remoteControlAtStartup` is a CLI settings key, and its meaning can change, as can the routine API's beta header.
- **Amends** ADR-0060 §3 once step 2 is accepted.
