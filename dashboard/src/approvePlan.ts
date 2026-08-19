import { client } from "./connectClient";

// What switching a session to `auto` actually grants (docs/adr/0053). One
// string because three surfaces confirm it — the plan card's "approve + auto",
// the actions menu's mode picker, and the new-session dialog's launch-mode
// picker — and a stale copy on any of them would be a human agreeing to the
// wrong thing.
//
// It used to say "only rm and sudo still come to you", which is what the ADR
// summary says and not what the code does. worker/src/session.ts's
// autoModeStillAsks is `ExitPlanMode || rm/sudo`, and everything else in
// FLEET_ASK_RULES — git push, gh, kubectl, curl, wget, env — is answered by
// the worker in auto. Naming them matters: "every result is a PR, and
// approving it is the review" stops being true the moment git push is
// unattended, and env is what puts GH_TOKEN in a rendered transcript.
export const AUTO_MODE_WARNING =
  "The agent stops asking for the rest of this session — including git push, gh, kubectl, curl, wget and env. Only rm and sudo still come to you, and so does the next plan.";

// The launch/switch modes the dashboard offers, shared so the actions menu and
// the new-session dialog cannot drift apart on what exists or what it is
// called. `auto` carries confirm: it is the one that grants authority.
export const PERMISSION_MODES = [
  { value: "default", label: "Default" },
  { value: "plan", label: "Plan" },
  { value: "acceptEdits", label: "Accept edits" },
  { value: "auto", label: "Auto", confirm: true },
] as const;

// Plan approval, the way the CLI's own ExitPlanMode menu does it (docs/adr/
// 0052): approving is a mode transition plus an answer, not just an answer.
// The CLI offers "Yes, and use auto mode" / "Yes, manually approve edits" /
// "No, keep planning"; this is the first two.
//
// Order is load-bearing. The mode has to be live BEFORE the approval is
// answered, or the turn that starts the moment the agent's canUseTool promise
// resolves runs in the old mode — which for a plan approval is "plan", where
// every write is refused.
//
// The second call is deliberately unconditional. The worker treats an
// incoming permission_mode entry as answering whatever is pending
// (resolveAllPendingAllow in worker/src/session.ts), so respondToPermission
// may find nothing left to resolve — it still writes the PERMISSION_RESPONSE
// every surface reads as "this plan was approved", and the dashboard's own
// optimistic echo is built on that entry existing.
export async function approvePlan(sessionId: string, seq: bigint, mode?: "auto"): Promise<void> {
  if (mode) await client.setPermissionMode({ sessionId, mode });
  await client.respondToPermission({
    sessionId,
    seq,
    decisionJson: JSON.stringify({ behavior: "allow" }),
  });
}
