import { useState } from "react";
import { ToolInputView } from "./ToolInputView";

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
  decision?: "allow" | "deny" | "interrupted";
  // The human's own words when they denied — parsed from the
  // PERMISSION_RESPONSE payload and, until now, never shown anywhere, so a
  // denial read as an unexplained refusal in the one place its reason
  // matters.
  denyMessage?: string;
  onAllow: () => void;
  onDeny: (message: string) => void;
  edgeClassName: string;
}) {
  const [reason, setReason] = useState("");
  const [isExpanded, setIsExpanded] = useState(false);

  if (!pending) {
    return (
      <button
        type="button"
        onClick={() => setIsExpanded(!isExpanded)}
        className="flex items-start gap-1.5 text-[10.5px] text-base-content/40 hover:text-base-content/60 w-full text-left group"
      >
        <span className="badge badge-ghost badge-xs flex-none">
          {tool} {decision === "deny" ? "denied" : decision === "interrupted" ? "interrupted" : "allowed"}
        </span>
        {isExpanded ? (
          <div className="flex-1 min-w-0">
            <ToolInputView tool={tool} input={input} />
            {denyMessage && <div className="text-[10.5px] text-error/70 mt-1">reason: {denyMessage}</div>}
          </div>
        ) : (
          <span className="truncate flex-1">{denyMessage || "permission request"}</span>
        )}
        <span className="text-[10px] flex-none group-hover:text-base-content/60">
          {isExpanded ? "▴" : "▾"}
        </span>
      </button>
    );
  }

  return (
    <div className={`border-y-2 border-primary/40 bg-primary/[0.04] py-4 ${edgeClassName}`}>
      <div className="text-[10px] tracking-[0.12em] font-semibold text-primary mb-2">
        PERMISSION — {tool.toUpperCase()}
      </div>
      <div className="max-h-[30vh] overflow-y-auto">
        <ToolInputView tool={tool} input={input} />
      </div>
      <div className="flex items-center gap-2 mt-3">
        <button type="button" className="btn btn-success btn-sm" disabled={busy} onClick={onAllow}>
          {busy ? (
            <>
              <span className="loading loading-spinner loading-xs"></span>
              Allowing...
            </>
          ) : (
            "Allow"
          )}
        </button>
        <button
          type="button"
          className="btn btn-outline btn-sm"
          disabled={busy}
          onClick={() => {
            onDeny(reason || "denied");
            setReason("");
          }}
        >
          {busy ? (
            <>
              <span className="loading loading-spinner loading-xs"></span>
              Denying...
            </>
          ) : (
            "Deny"
          )}
        </button>
        <input
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && reason.trim()) {
              onDeny(reason);
              setReason("");
            }
          }}
          placeholder="why not? (optional)"
          disabled={busy}
          className="flex-1 bg-transparent border border-base-content/15 rounded-lg px-3 py-2 text-[12px] outline-none focus:border-primary/50"
        />
      </div>
    </div>
  );
}
