import { useState } from "react";
import { ActionButton } from "./ActionButton";
import { client } from "../connectClient";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { type ListSummary } from "../transcript";
import { DiffLines } from "./DiffLines";
import { QuestionCard } from "./QuestionCard";
import { Markdown } from "./Markdown";
import { summarizeToolInput } from "../transcript";
import { approvePlan } from "../approvePlan";
import { ConfirmModal } from "./ConfirmModal";
import { TextResponseModal } from "./TextResponseModal";
import { PlanModal } from "./PlanModal";

// The pending decision, answerable from the list.
//
// This is the console rewrite's central move: a blocked session used to say only
// that *a* decision was waiting, so answering one always cost a navigation and a
// read. A blocked session is stalled until a human clicks, which makes that
// latency the product's real cost (docs/dashboard-spec.md §2, §8 item 3).
//
// It calls the RPCs directly rather than going through useSessionDetail's `run`:
// the list has no open session, and a per-card busy flag is all the state one
// button needs. `reload` refreshes App's poll so the card leaves the NEEDS YOU
// bucket immediately instead of on the next 5s tick.

type Layout = "wide" | "stacked";

// `pending` names the button whose call is in flight, so the spinner lands on
// the one that was clicked rather than every button dimming together.
//
// `answered` is the seq of a decision this list row has already sent. The
// answer takes a round trip, and then reload() takes another before the row's
// summary stops reporting the decision as pending — a window in which the same
// allow/deny buttons sat there, fully live, inviting a second click on a
// question that was already answered.
function useDecision(reload: () => void) {
  const [pending, setPending] = useState<string | null>(null);
  const [answered, setAnswered] = useState<bigint | null>(null);
  const [error, setError] = useState<string | null>(null);
  const send = (key: string, action: () => Promise<unknown>, decidedSeq?: bigint) => {
    setPending(key);
    setError(null);
    action()
      .then(() => {
        if (decidedSeq !== undefined) setAnswered(decidedSeq);
        reload();
      })
      .catch((err: Error) => setError(err.message))
      .finally(() => setPending(null));
  };
  return { busy: pending !== null, pending, answered, error, send };
}

// Edit/Write render as a real diff; anything else shows its one-line summary.
// A permission prompt whose content is unreadable trains people to approve
// without looking, which is worse than no prompt at all.
function PermissionBody({ tool, input }: { tool: string; input: unknown }) {
  const i = (input ?? {}) as { file_path?: string; old_string?: string; new_string?: string; content?: string };
  const path = i.file_path;
  const isDiff = (tool === "Edit" || tool === "Write") && typeof (i.new_string ?? i.content) === "string";

  return (
    <>
      <div className="text-xs text-dim tracking-[0.05em] break-all">
        PERMISSION · {tool}
        {path && (
          <>
            {" · "}
            <span className="text-base-content">{path}</span>
          </>
        )}
      </div>
      <div className="mt-2">
        {isDiff ? (
          <DiffLines before={i.old_string ?? ""} after={(i.new_string ?? i.content) as string} maxLines={4} />
        ) : (
          <div className="border border-line bg-code px-2.5 py-[3px] text-sm text-text2 overflow-x-auto whitespace-pre-wrap break-all">
            {summarizeToolInput(input)}
          </div>
        )}
      </div>
    </>
  );
}

export function DecisionInline({
  session,
  summary,
  reload,
  layout = "wide",
  // Mobile lets you defer a question without answering it. Purely local: the
  // agent stays blocked, this human just isn't dealing with it now.
  onAskLater,
  // Dismissing a proposal soft-deletes the session, so a caller showing only that
  // session (the detail view) has to let go of it rather than sit on a dead row.
  onDismissed: _onDismissed,
}: {
  session: Session;
  summary?: ListSummary;
  reload: () => void;
  layout?: Layout;
  onAskLater?: () => void;
  onDismissed?: () => void;
}) {
  const { busy, pending, answered, error, send } = useDecision(reload);
  const [reason, setReason] = useState("");
  // "deny" reveals the reason field; it is not shown up front. See `actions`.
  const [denying, setDenying] = useState(false);
  const [autoConfirmOpen, setAutoConfirmOpen] = useState(false);
  const [feedbackOpen, setFeedbackOpen] = useState(false);
  const [planOpen, setPlanOpen] = useState(false);
  const stacked = layout === "stacked";
  // One padding value and one gap for every decision kind. They were px-4/pb-4
  // with gaps of 12, 10 and 10 depending on which branch rendered, so three
  // cards in a row sat on three different rhythms.
  const pad = stacked ? "px-3.5 pt-1 pb-3.5" : "px-4 pt-1 pb-4";

  // Touch targets: the mobile mockup's allow/deny are ~44px tall, because this
  // is the surface most likely used to unblock a session away from a desk.
  const primaryBtn = `text-center font-semibold cursor-pointer bg-primary text-primary-content disabled:opacity-50 ${
    stacked ? "py-3 text-base flex-1" : "py-2 px-6 text-base"
  }`;
  const secondaryBtn = `text-center cursor-pointer border border-acc-line hover:border-error hover:text-error disabled:opacity-50 ${
    stacked ? "py-3 text-base flex-1" : "py-2 px-6 text-base"
  }`;

  // A proposal has no transcript yet — the decision is whether to dispatch at
  // all. Dismiss is a soft delete, which drops the row out of the alert dedup
  // index, so a still-firing alert is proposed again next fire: "not now", not
  // "never".
  // The inline proposal-approval card is gone. A proposal is no longer a
  // session with a status — it is a row in its own table with its own view
  // (docs/adr/0048), so there is no proposed session for this component to
  // render a decision for.

  const permission = summary?.pendingPermission ?? null;
  const questionEntry = summary?.pendingQuestion ?? null;

  // Answered, but the summary still says pending — the answer is on the wire
  // and reload() hasn't come back yet. Keyed by the decision's own seq, so a
  // *different* decision arriving next renders normally with no reset logic.
  if (answered !== null && (permission?.entry.seq === answered || questionEntry?.seq === answered)) {
    return (
      <div className={`${pad} flex items-center gap-2 text-sm text-dim`}>
        <span className="loading loading-spinner loading-xs flex-none" />
        sent — waiting for the agent to pick it up
      </div>
    );
  }

  if (permission) {
    const isPlan = permission.tool === "ExitPlanMode";
    const respond = (behavior: "allow" | "deny", message?: string) =>
      send(
        behavior,
        () =>
          client.respondToPermission({
            sessionId: session.id,
            seq: permission.entry.seq,
            decisionJson: JSON.stringify({ behavior, message }),
          }),
        permission.entry.seq,
      );

    // A plan is prose, not a diff — deciding on it means reading it, so the list
    // sends you into the session rather than pretending a 3-line preview is
    // enough to approve on.
    //
    // It still has to be *readable* prose, though. This used to join the first
    // four lines into one paragraph and clamp that to three lines, which on a
    // phone showed about a sentence and a half — with the raw `## ` heading
    // markers still in it, since it never went through the markdown renderer.
    // Approving from a teaser like that is approving blind. Now it renders as
    // markdown in a scroll box: enough to actually judge a short plan from the
    // list, and "read it first" still exists for a long one.
    if (isPlan) {
      const plan = (permission.input as { plan?: string } | undefined)?.plan ?? "";
      return (
        <div className={`${pad} flex flex-col gap-3`}>
          <div className="text-xs text-dim tracking-[0.05em]">PLAN · waiting for approval</div>
          {/* The preview is itself the way in — clicking the plan you are
              trying to read is the gesture people try first. */}
          <button
            type="button"
            onClick={() => setPlanOpen(true)}
            title="Read the full plan"
            className={`text-sm text-text2 overflow-y-auto text-left cursor-pointer ${
              stacked ? "max-h-[40vh]" : "max-h-[30vh]"
            }`}
          >
            <Markdown text={plan} />
          </button>
          {error && <div className="text-xs text-error">{error}</div>}
          {/* Same menu the dock's PlanCard offers, so approving from the list
              is not a lesser decision than approving from the session — auto
              behind a confirm, plain approve beside it (docs/adr/0052). */}
          <div className={`flex flex-wrap gap-2.5 ${stacked ? "" : "items-center"}`}>
            <ActionButton
              busy={pending === "allow"}
              disabled={busy}
              className={primaryBtn}
              onClick={() => setAutoConfirmOpen(true)}
            >
              approve + auto
            </ActionButton>
            <button
              type="button"
              disabled={busy}
              onClick={() =>
                send("allow", () => approvePlan(session.id, permission.entry.seq), permission.entry.seq)
              }
              className={secondaryBtn}
            >
              approve only
            </button>
            <button type="button" disabled={busy} onClick={() => setFeedbackOpen(true)} className={secondaryBtn}>
              request changes
            </button>
            <button type="button" onClick={() => setPlanOpen(true)} className={secondaryBtn}>
              read in full
            </button>
          </div>
          <PlanModal
            open={planOpen}
            plan={plan}
            busy={busy}
            pending={pending}
            onApproveAuto={() => {
              setPlanOpen(false);
              setAutoConfirmOpen(true);
            }}
            onApprove={() => {
              setPlanOpen(false);
              send("allow", () => approvePlan(session.id, permission.entry.seq), permission.entry.seq);
            }}
            onRequestChanges={() => {
              // Closed first: the feedback dialog is a second <dialog>, and
              // stacking it on this one is the nesting Modal.tsx guards.
              setPlanOpen(false);
              setFeedbackOpen(true);
            }}
            onClose={() => setPlanOpen(false)}
          />
          {/* The third answer to a plan, which the list did not offer: neither
              approve nor walk away, but say what to change. Posted as an
              ordinary message — the worker treats a human reply arriving while
              a permission is pending as deny-with-feedback and re-arms the
              plan, so this needs no separate RPC (see PlanCard, which takes the
              same path from the session view). */}
          <TextResponseModal
            open={feedbackOpen}
            title="REQUEST CHANGES"
            context="Sent to the agent as a reply. It supersedes the plan and the agent revises it."
            placeholder="What should change before this is approved?"
            submitLabel="send"
            busy={busy}
            onSubmit={(text) => send("deny", () => client.postMessage({ sessionId: session.id, text }), permission.entry.seq)}
            onClose={() => setFeedbackOpen(false)}
          />
          <ConfirmModal
            open={autoConfirmOpen}
            title="Approve and switch to auto mode?"
            message="A model classifier answers the ordinary permission prompts for the rest of this session. git push, gh, rm, sudo, kubectl, curl, wget and env still come to you, and so does the next plan."
            confirmLabel="approve + auto"
            danger={false}
            onConfirm={() => {
              setAutoConfirmOpen(false);
              send("allow", () => approvePlan(session.id, permission.entry.seq, "auto"), permission.entry.seq);
            }}
            onCancel={() => setAutoConfirmOpen(false)}
          />
        </div>
      );
    }

    // One row along the bottom, not a 320px column pinned beside the payload.
    // That column reserved its full width whatever the decision needed, so a
    // one-line Bash sat next to an empty third of the card, and the reason box
    // occupied the same space whether or not anyone was going to deny.
    //
    // The reason field is now behind "deny": typing a reason is a thing you do
    // *after* deciding to refuse, and rendering it up front asked every reader
    // to skip a text input to reach the two buttons they actually wanted.
    const actions = (
      <div className="flex flex-col gap-2">
        {error && <div className="text-xs text-error">{error}</div>}
        {denying ? (
          <div className="flex items-center gap-2 flex-wrap">
            <input
              value={reason}
              autoFocus
              onChange={(e) => setReason(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  respond("deny", reason.trim() || "denied");
                  setReason("");
                  setDenying(false);
                }
                if (e.key === "Escape") setDenying(false);
              }}
              disabled={busy}
              placeholder="why? — sent to the agent, or just press enter"
              aria-label="denial reason"
              className="border border-line px-2.5 py-[7px] text-xs bg-transparent outline-none focus:border-primary/60 placeholder:text-dim2 flex-1 min-w-0"
            />
            <ActionButton
              busy={pending === "deny"}
              disabled={busy}
              className={secondaryBtn}
              onClick={() => {
                respond("deny", reason.trim() || "denied");
                setReason("");
                setDenying(false);
              }}
            >
              send deny
            </ActionButton>
            <button type="button" onClick={() => setDenying(false)} className="text-xs text-dim2 hover:text-dim cursor-pointer">
              cancel
            </button>
          </div>
        ) : (
          <div className="flex items-center gap-2.5 flex-wrap">
            <ActionButton busy={pending === "allow"} disabled={busy} className={primaryBtn} onClick={() => respond("allow")}>
              allow
            </ActionButton>
            <button type="button" disabled={busy} onClick={() => setDenying(true)} className={secondaryBtn}>
              deny
            </button>
          </div>
        )}
      </div>
    );

    return (
      <div className={`${pad} flex flex-col gap-3`}>
        <PermissionBody tool={permission.tool} input={permission.input} />
        {actions}
      </div>
    );
  }

  if (questionEntry) {
    // Reuse the session-detail QuestionCard so the list answers any batch
    // (multi-question, free-text) identically — no bespoke single-question
    // restriction, and the answer no longer costs a navigation.
    const submit = (answers: Record<string, string>) =>
      send(
        "answer",
        () =>
          client.answerQuestion({
            sessionId: session.id,
            seq: questionEntry.seq,
            answersJson: JSON.stringify({ answers }),
          }),
        questionEntry.seq,
      );

    return (
      <div className={`${pad} flex flex-col gap-3`}>
        <QuestionCard entry={questionEntry} answer={null} busy={busy} compact={stacked} embedded onSubmit={submit} />
        {error && <div className="text-xs text-error">{error}</div>}
        <div className={`flex gap-3.5 ${stacked ? "justify-center" : "items-center"}`}>
          {onAskLater && (
            <button type="button" onClick={onAskLater} className="text-xs text-dim2 hover:text-dim cursor-pointer py-1">
              ask me later
            </button>
          )}
        </div>
      </div>
    );
  }

  // awaiting_human is set by core the moment a decision is appended, so this is
  // the brief window before this session's transcript fetch has caught up — or a
  // decision shape the list can't render. Either way: say so. The card's title
  // is the way into the session, here as on every other row — a second "open
  // session" control under every decision was the same destination said twice.
  return (
    <div className={`${pad} flex flex-col gap-2`}>
      <div className="text-sm text-dim">A decision is waiting in this session.</div>
    </div>
  );
}
