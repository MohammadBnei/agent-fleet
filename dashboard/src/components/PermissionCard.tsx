import { useEffect, useState } from "react";
import type { PermissionOutcome } from "../transcript";
import { ActionButton } from "./ActionButton";
import { ToolInputView } from "./ToolInputView";

// A remembered allow (docs/adr/0076 §3): core renders the rule from the
// request, and the card shows that exact rule before the click.
export type Grant = {
  scope: "session" | "definition";
  ruleForm: "exact" | "prefix";
};
export type GrantRules = { exact: string; prefix: string };
export type GrantPreview = (seq: bigint) => Promise<GrantRules>;

// Generic renderer for any PERMISSION_REQUEST entry canUseTool posts —
// the fleet-wide replacement for docs/adr/0021's Write/Edit-absent-from-
// allowedTools gate. ExitPlanMode's request gets the nicer, plan-specific
// PlanCard instead (its input is a full plan worth distinct rendering);
// this handles every other tool, uniformly, since canUseTool itself does
// zero tool-specific classification anymore — the SDK's own permission
// mode already decided this call was worth asking about.
//
// Allow/Deny are both single-click, matching the plain CLI Yes/No prompt —
// no intermediate "open a reason box, then Send" step. The reason input
// stays inline and optional the whole time; Deny reads whatever's in it
// (or falls back to a generic message) rather than gating on it being
// opened first.
//
// `edgeClassName` breaks the card out of the feed's own horizontal padding
// so it can span full width — desktop/mobile pass their own negative-margin
// pair since their feed containers use different padding (mirrors PlanCard).
export function PermissionCard({
  tool,
  input,
  pending,
  busy,
  decision,
  denyMessage,
  onAllow,
  onDeny,
  edgeClassName,
  preview,
}: {
  tool: string;
  input: unknown;
  pending: boolean;
  busy: boolean;
  // The real recorded outcome once resolved (transcript.ts's
  // resolvedPermissionDecisions) — "interrupted" for a request that was
  // never explicitly answered but got swept up by a later Kill/Interrupt
  // (no PERMISSION_RESPONSE ever posted for those, see that function's own
  // comment); undefined only for a pre-fix legacy row with no matching
  // response yet, where "allowed" stays the fallback.
  // "withdrawn" is one closed unanswered from the decision dock.
  decision?: PermissionOutcome;
  // The human's own words when they denied — parsed from the
  // PERMISSION_RESPONSE payload and, until now, never shown anywhere, so a
  // denial read as an unexplained refusal in the one place its reason
  // matters.
  denyMessage?: string;
  onAllow: (grant?: Grant) => void;
  onDeny: (message: string) => void;
  edgeClassName: string;
  // Set for a definition session's pending request: offers "allow for this
  // session" and "allow for this definition" with the rule each would store.
  preview?: () => Promise<GrantRules>;
}) {
  const [reason, setReason] = useState("");
  const [rules, setRules] = useState<GrantRules | null>(null);
  const [wide, setWide] = useState(false);
  useEffect(() => {
    if (!pending || !preview) return;
    let live = true;
    // No rules (a request no grant could cover) just means no remember row.
    preview().then(
      (r) => live && setRules(r),
      () => {},
    );
    return () => {
      live = false;
    };
    // Callers pass a fresh closure every render; the request at this seq
    // does not change, so fetch once per pending card.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pending, !!preview]);
  const form = wide && rules?.prefix ? "prefix" : "exact";
  const rule = rules ? (form === "prefix" ? rules.prefix : rules.exact) : "";
  const [denying, setDenying] = useState(false);
  const [isExpanded, setIsExpanded] = useState(false);
  const sendDeny = () => {
    onDeny(reason.trim() || "denied");
    setReason("");
    setDenying(false);
  };

  if (!pending) {
    return (
      <button
        type="button"
        onClick={() => setIsExpanded(!isExpanded)}
        className="flex items-start gap-2 text-xs text-dim2 hover:text-dim w-full text-left group"
      >
        <span
          className={`flex-none border px-1 ${
            decision === "deny" ||
            decision === "interrupted" ||
            decision === "withdrawn"
              ? "border-pink-line text-error"
              : "border-line text-dim"
          }`}
        >
          {tool}{" "}
          {decision === "deny"
            ? "denied"
            : decision === "interrupted"
              ? "interrupted"
              : decision === "withdrawn"
                ? "closed"
                : "allowed"}
        </span>
        {isExpanded ? (
          <div className="flex-1 min-w-0">
            <ToolInputView tool={tool} input={input} />
            {denyMessage && (
              <div className="text-xs text-error mt-1">
                reason: {denyMessage}
              </div>
            )}
          </div>
        ) : (
          <span className="truncate flex-1">
            {denyMessage || "permission request"}
          </span>
        )}
        <span className="text-2xs flex-none group-hover:text-dim">
          {isExpanded ? "▴" : "▾"}
        </span>
      </button>
    );
  }

  return (
    <div
      className={`border-y border-pink-line bg-pink-bg py-4 ${edgeClassName}`}
    >
      <div className="flex items-center gap-2 mb-2.5">
        <span className="w-1.5 h-1.5 rounded-full bg-error animate-fpulse flex-none" />
        <span className="text-2xs tracking-[0.1em] text-error">
          ◉ PERMISSION REQUEST · {tool.toUpperCase()}
        </span>
      </div>
      <div className="max-h-[30vh] overflow-y-auto">
        <ToolInputView tool={tool} input={input} />
      </div>
      {/* Same interaction as the list's DecisionInline: deny reveals the
          reason, Enter sends it, an empty reason sends "denied". The dock used
          to deny on one click with the reason field always up, so the same
          permission was refused two different ways depending on where you
          answered it. */}
      {denying ? (
        <div className="flex items-center gap-2.5 mt-3.5 flex-wrap">
          <input
            value={reason}
            autoFocus
            onChange={(e) => setReason(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") sendDeny();
              if (e.key === "Escape") {
                setReason("");
                setDenying(false);
              }
            }}
            placeholder="why? — sent to the agent, or just press enter"
            aria-label="denial reason"
            disabled={busy}
            className="flex-1 min-w-[180px] bg-transparent border border-line px-3 py-2 text-sm outline-none focus:border-primary/60 placeholder:text-dim2"
          />
          <ActionButton
            className="border border-acc-line px-6 py-2 text-base hover:border-error hover:text-error disabled:opacity-50"
            busy={busy}
            onClick={sendDeny}
          >
            send deny
          </ActionButton>
          <button
            type="button"
            onClick={() => {
              setReason("");
              setDenying(false);
            }}
            className="text-xs text-dim2 hover:text-dim cursor-pointer"
          >
            cancel
          </button>
        </div>
      ) : (
        <div className="flex items-center gap-2.5 mt-3.5 flex-wrap">
          <ActionButton
            className="bg-primary text-primary-content px-6 py-2 text-base font-semibold disabled:opacity-50"
            busy={busy}
            onClick={() => onAllow()}
          >
            allow
          </ActionButton>
          <button
            type="button"
            disabled={busy}
            onClick={() => setDenying(true)}
            className="border border-acc-line px-6 py-2 text-base hover:border-error hover:text-error disabled:opacity-50 cursor-pointer"
          >
            deny
          </button>
        </div>
      )}
      {!denying && rules && (
        <div className="flex items-center gap-2 mt-2.5 flex-wrap text-xs text-dim">
          <span>remember</span>
          <code className="text-base-content break-all">{rule}</code>
          {rules.prefix && (
            <label className="flex items-center gap-1 cursor-pointer">
              <input
                type="checkbox"
                checked={wide}
                disabled={busy}
                onChange={(e) => setWide(e.target.checked)}
              />
              wider: covers more than this call
            </label>
          )}
          <ActionButton
            className="border border-line px-3 py-1 hover:border-primary disabled:opacity-50"
            busy={busy}
            onClick={() => onAllow({ scope: "session", ruleForm: form })}
          >
            allow for this session
          </ActionButton>
          <ActionButton
            className="border border-line px-3 py-1 hover:border-primary disabled:opacity-50"
            busy={busy}
            onClick={() => onAllow({ scope: "definition", ruleForm: form })}
          >
            allow for this definition
          </ActionButton>
        </div>
      )}
    </div>
  );
}
