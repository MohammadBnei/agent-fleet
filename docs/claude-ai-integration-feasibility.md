# Feasibility: Claude Code on claude.ai, opened from the fleet

- **Date:** 2026-11-06
- **Status:** Study. The decision it leads to is [ADR-0083](adr/0083-claude-ai-sessions.md) (Proposed)
- **Question:** can the console open Claude Code sessions hosted on claude.ai (claude.ai/code and the mobile app) "directly", ideally embedded in frames?
- **Account:** a personal Max/Pro subscription, the same `CLAUDE_CODE_OAUTH_TOKEN` every pod runs on (ADR-0060). It is not Team or Enterprise.

## TL;DR

- **Frames: no.** claude.ai refuses to be framed, and the fleet refused framing twice already. "Open" means a link to a new tab.
- **Two real directions:**
  - **A.** The fleet *launches* a claude.ai cloud session and links to it.
  - **B.** A fleet pod *registers* with claude.ai through Remote Control, so a fleet session also shows up in claude.ai/code and the mobile app.
- **Recommendation: B.** Make it opt-in for repo sessions, and run a spike first. B keeps the transcript, the lease, the slot cap and the vault. A gives up all four.
- **A is worth only a link-out.** It amounts to "hand this off to claude.ai", with no lifecycle integration.
- **A third option, C (self-hosted environments), is not available on this plan.**

## 1. The frame question

Checked from a cloud session on 2026-11-06 (`curl -D - https://claude.ai/code`, also `https://claude.ai/`):

```
x-frame-options: SAMEORIGIN
cross-origin-opener-policy: same-origin
cross-origin-embedder-policy: require-corp
content-security-policy: ... frame-src 'self' https://challenges.cloudflare.com blob: ...
```

That response was a Cloudflare challenge page (403). The real app was not reached, because the fetch carried no browser session. It is still the documented posture, and nothing in Anthropic's docs offers an embed widget. Even if the headers allowed framing, three more problems would remain:

- **Third-party cookie.** The claude.ai login cookie would be third-party inside a `fleet.bnei.dev` frame, and browsers block those.
- **Same-site precedent.** That is exactly why [ADR-0068](adr/0068-islands.md) rejected `/embed/*` iframes in favour of same-site web components. Islands work only because host and fleet are both on `bnei.dev`. claude.ai is not.
- **Grafana precedent.** [ADR-0047](adr/0047-metrics-scoped-to-the-hubs.md) also rejected framing Grafana, for its own reason: a frame cannot link into the console, so you cannot click a cell and land on that session.

Core's own `frame-ancestors 'none'` / `X-Frame-Options: DENY` (`core/internal/auth/headers.go`) decides only who may frame *the console*, not what the console may frame. It is not the obstacle and needs no change.

**Conclusion: a link with `target="_blank" rel="noreferrer"`, the console's existing pattern for external objects.** It already exists for PR links in `TranscriptEntryView.tsx`, card subjects in `CardBody.tsx`, Grafana in `Observability.tsx`, and alerts in `Schedules.tsx`. The natural place for the link is the SESSION facts block of `SessionPanels.tsx`, next to id, model, mode and release.

## 2. Option A: the fleet launches claude.ai cloud sessions

### Mechanism (documented)

There are two documented ways to start a session.

- **Routines** ([code.claude.com/docs/en/routines](https://code.claude.com/docs/en/routines.md)):
  1. A routine is created once in the claude.ai web UI and gets a trigger token.
  2. Core would `POST https://api.anthropic.com/v1/claude_code/routines/<trig_id>/fire`, with `Authorization: Bearer <token>` and `anthropic-beta: experimental-cc-routine-2026-04-01`. The optional body `{"text": "..."}` carries the prompt.
  3. The response carries `claude_code_session_url`.

  Limits:
  - 100 API fires per hour per account.
  - The docs say the endpoint is for claude.ai users and *not* part of the Platform API, so it is beta-gated and can change.
  - Each fire starts a *new* session. It is not a follow-up.
- **The CLI** ([code.claude.com/docs/en/claude-code-on-the-web](https://code.claude.com/docs/en/claude-code-on-the-web.md)):
  - `claude --cloud "<prompt>"` starts a session on the current repo's GitHub remote.
  - `claude -p "<msg>" --cloud <session-id> --output-format json` sends a follow-up and returns `{ok, session_id, url}`.

  It needs a CLI signed in with `claude auth login` to a subscription. API keys are refused. Core is Go and holds no Claude credential, so this would mean a CLI process somewhere with the account's login.

**Not documented, so not to build on:**
- a public API to list sessions, read their transcript or events, or get webhooks;
- a `claude.ai/code?prompt=` deep link (`claude-cli://open` opens a local terminal, not the web).

The session-management tools visible inside claude.ai/code itself (create/list/send) are not a public API.

### What it would take

- A new optional field on the session row and `Session` proto, for example `external_url` (a new name and number: `kind`, 15 and 16 are reserved, `core.proto`), plus a migration.
- One config secret (the routine token) in Infisical, read by core.
- A "Hand off to claude.ai" action and a link in `SessionPanels.tsx`.

### What the fleet gives up

| Fleet property | With a claude.ai cloud session |
|---|---|
| Durable `transcript`, pull/cursor (ADR-0013) | None. Nothing can be read back. The console has only a URL |
| Decisions answered in the console (ADR-0042) | Answered in claude.ai. The DecisionDock never sees them |
| `pod_phase`, reconcile, idle and retention sweeps | None. Core cannot tell running from finished |
| Slot cap (`MAX_IN_FLIGHT_TASKS`) | Does not apply. Anthropic's compute, the subscription's own limits |
| Credentials through Agent Vault (ADR-0065/0075) | Bypassed. claude.ai uses its own GitHub App and the account's login |
| Fleet trailers (`worker/git-hooks`) | Absent. Commits carry whatever claude.ai writes. The identity rule (`ukubi-claude-cloud`) is a convention there, and `trailers.yml` catches a mistake only at PR time |
| Context packs, `/genome`, fleet-shared skills | Absent. Only the repo's own `CLAUDE.md` and `.claude/` load |

The only way back is indirect: the session's PR produces GitHub webhooks, and those reach the fleet through `ukubi-bridge`, the same as any PR.

**The link may live only in the dashboard.** `check-trailers.py` refuses claude.ai URLs in commits and PR text by design, so the fleet must never write it to git.

### Against VISION

- **"Propose, never author" / human gate:** met. The owner starts the session from the console.
- **"Every result is a PR":** met, but unchecked, as it is today.
- **"Self-contained substrate":** not met. The compute and the transcript live at Anthropic, and the fleet cannot observe them.

**Verdict:** feasible for a narrow use, "start this on claude.ai instead and give me the link". It is not feasible as a fleet backend, because the fleet would carry a row it cannot reason about.

## 3. Option B: fleet pods through Remote Control

### Mechanism

- **What Remote Control does** ([code.claude.com/docs/en/remote-control](https://code.claude.com/docs/en/remote-control.md)): it registers a running Claude Code process with claude.ai, so the session is listed in claude.ai/code and the mobile app and can be driven from there. Tools and files stay on the machine. The process polls `api.anthropic.com` over outbound HTTPS, with no inbound port.
- **Plan:** Pro and Max are supported. API keys are not.
- **The setting already exists in the worker.** `remoteControlAtStartup` is a key the pinned CLI reads, and the worker passes it today, deliberately `false` (`worker/src/session.ts`, pinned by `session.test.ts`, [ADR-0060](adr/0060-claude-codes-own-cross-session-channel-is-open.md) §3: "phone/web control of the pod, a different channel nobody asked for").
- **The change itself is one setting.** The work is in what surrounds it.

### What the fleet keeps

- The pod still runs on ukubi-cluster, in its own PVC, under the lease, the slot cap and the idle sweep.
- `api.anthropic.com` is already one of the three hosts the sidecar routes through Agent Vault (ADR-0075 §7). The Remote Control polling goes to the same host.
- The transcript, the console and Discord keep working as today, with claude.ai as an extra *view*.

### What it would take

1. **An opt-in.** The setting would be on for a repo session only when asked, for example a `remote_control` flag on the session, set from the New Session dialog. Never on for definition pods: [ADR-0064](adr/0064-definition-pod-profile.md) already closes Claude Code's own channels there, and a definition pod's tool surface is a reviewed ceiling that a phone must not widen.
2. **Learning the URL.** The worker reports the Remote Control session id or URL once it is known (sidecar → `CoreService`, lease-authenticated, a new method in the per-method table, ADR-0057). Core stores it in the same new `Session` field as in option A and the console shows it as a link.
3. **An ADR amending ADR-0060 §3.**

### Unknowns: a spike must settle these before any design

| # | Unknown | Why it matters | How the spike answers it |
|---|---|---|---|
| U1 | Does Remote Control start under the Agent SDK `query()` in streaming-input mode, or only in the interactive CLI / `claude remote-control`? | The worker is SDK-only | `/kind-local` worker with the setting `true`. Look for the session in claude.ai/code |
| U2 | Does the first-run `Enable Remote Control? (y/n)` consent block a headless pod, or does `remoteControlAtStartup: true` count as consent? | A prompt nobody can answer wedges the pod silently | Same run: watch the worker's stderr and whether the SDK reaches `init` |
| U3 | Does the token the fleet holds carry the scope Remote Control needs? `CLAUDE_CODE_OAUTH_TOKEN` comes from `claude setup-token` and may be inference-only. The pod also holds only a placeholder that the vault swaps on the wire | Likely blocker. If registration needs a full claude.ai login, B needs a different credential | Same run. If it fails, check whether the refusal names a scope |
| U4 | How does a permission prompt shown on the phone interact with `canUseTool` and `DECISION_WAIT_MS`? Does claude.ai even see a prompt that `canUseTool` holds? Can both the phone and the console answer it? | A second decision channel around the transcript: the class of gap ADR-0060 accepted for `SendMessage`, but for permissions | Trigger a gated `Bash` (`rm`) and answer it from each side in turn |
| U5 | Is a message typed on the phone written to the `transcript`? | If not, a pod driven from the phone looks idle in the console, and the idle sweep may tear it down mid-turn (ADR-0060's open question, again) | Send a message from the phone and read the `transcript` table |
| U6 | Does the claude.ai view survive warm/resume (a new pod, the same `RESUME_SESSION_ID`)? | Sessions outlive pods (ADR-0029). The URL may change every warm | Stop and warm the session, then compare the ids |

**U3 decides the option.** If the fleet's token cannot register, B costs a second credential holding a full claude.ai login, and that changes the security story.

### Against VISION and the locked decisions

- **Self-contained substrate, hub-and-spoke, vault:** kept.
- **"A decision is always a real structured `RespondToPermission`":** at risk through U4. If a phone answer resolves a prompt without a transcript entry, that is a decision the fleet never recorded. The ADR must say whether that is accepted, as 0060 accepted `SendMessage`, or fenced: for example, Remote Control only in `plan` or with `canUseTool` still the sole decider.
- **Account-wide reach:** same as ADR-0060. Anyone signed in to the account can drive the pod. On a personal plan that is the owner alone.

## 4. Option C: self-hosted environments

Self-hosted environments ([code.claude.com/docs/en/self-hosted-environments](https://code.claude.com/docs/en/self-hosted-environments.md)):
- **What it is:** a runner in our network claims claude.ai sessions from Anthropic's queue and runs the Claude Code CLI on our nodes. It is the closest official match to "claude.ai sessions on ukubi-cluster".
- **Why it's out:**
  - It is **Team/Enterprise only** (public beta), so it is not available on this account.
  - It would duplicate the provisioner.
  - It would run the stock CLI, not the worker, so none of `canUseTool`, packs, the vault or trailers would apply.
- **Revisit** only if the account moves to Team/Enterprise.

## 5. Comparison

| | A: launch on claude.ai | B: Remote Control on fleet pods |
|---|---|---|
| Compute | Anthropic | ukubi-cluster pod |
| Transcript / console feed | None | Kept (U5: phone input may bypass it) |
| Decisions in the console | No | Yes, plus possibly the phone (U4) |
| Agent Vault, egress router | Bypassed | Kept |
| Slot cap, idle and retention sweeps | Not applicable | Kept |
| Fleet trailers, packs, fleet-shared | Absent | Kept |
| Opens in claude.ai/code and mobile | Yes | Yes |
| API stability | Beta header, "not Platform API", 100/h | A settings key the pinned CLI reads; re-check on every CLI bump (as 0060) |
| New surface | Routine token in core; one field; link | One field; one `CoreService` method; one opt-in; amend 0060 §3 |
| Main unknown | None (it works, but the fleet sees nothing) | U3: token scope |

## 6. Recommendation

1. **No iframe, ever.** Link to a new tab from the SESSION facts.
2. **Pursue B, behind a per-session opt-in for repo sessions only.** First run a `/kind-local` spike against U1 to U6. Nothing is built before the spike. If U3 fails, stop and bring the credential question back to the owner.
3. **A only as a link-out, if wanted at all.** A "Start on claude.ai" action fires the routine and stores the returned URL on a row marked external. There is no lifecycle and no pod, and the row says so. It can ship independently of B, but it buys little over opening claude.ai/code by hand.
4. **C:** revisit on a plan change.

Both directions need the same storage: one new nullable URL field on `sessions`/`Session`, rendered as a link and never written to git.
