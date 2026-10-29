import { Code, ConnectError } from "@connectrpc/connect";
import { client } from "./connectClient";

// What switching a session to `auto` actually grants (docs/adr/0053). One
// string, read by AutoModeConfirm (every switch and every plan approval into
// auto) and the new-session dialog's launch-mode hint — a stale copy on any of
// them is a human agreeing to the wrong thing, which is exactly what the list's
// plan approval did while it carried its own.
//
// It used to say "only rm and sudo still come to you", which is what the ADR
// summary says and not what the code does. In auto, worker/src/session.ts's
// autoModeStillAsksCommand asks for rm/sudo, ExitPlanMode asks only while the
// session is in plan mode (docs/adr/0082 §5), and everything else in
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
//
// One reply per prompt (migration 000015): when the worker's own "allowed by a
// permission-mode change" record lands first, this answer gets AlreadyExists.
// The plan is approved either way, so that is success.
//
// `api` is a parameter for the same reason as subscribeTranscript's
// openStream: the test's fake used to arrive through `mock.module`, which is
// process-wide in bun and leaked into connectClient.test.ts whenever the two
// files shared a run. Production callers pass three args.
type PlanApi = Pick<typeof client, "setPermissionMode" | "respondToPermission">;

export async function approvePlan(
  sessionId: string,
  seq: bigint,
  mode?: "auto",
  api: PlanApi = client,
): Promise<void> {
  if (mode) await api.setPermissionMode({ sessionId, mode });
  try {
    await api.respondToPermission({
      sessionId,
      seq,
      decisionJson: JSON.stringify({ behavior: "allow" }),
    });
  } catch (err) {
    if (mode && err instanceof ConnectError && err.code === Code.AlreadyExists)
      return;
    throw err;
  }
}
