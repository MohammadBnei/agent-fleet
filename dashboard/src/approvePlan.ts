import { client } from "./connectClient";

// What switching a session to `auto` actually grants (docs/adr/0053). One
// string because two surfaces confirm it — the plan card's "approve + auto"
// and the actions menu's mode picker — and a stale copy on either one would
// be a human agreeing to the wrong thing.
export const AUTO_MODE_WARNING =
  "The agent stops asking for the rest of this session. Only rm and sudo still come to you — and so does the next plan.";

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
