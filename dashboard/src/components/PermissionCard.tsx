import { useState } from "react";

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
  onAllow,
  onDeny,
  edgeClassName,
}: {
  tool: string;
  input: unknown;
  pending: boolean;
  busy: boolean;
  onAllow: () => void;
  onDeny: (message: string) => void;
  edgeClassName: string;
}) {
  const [reason, setReason] = useState("");
  const [isExpanded, setIsExpanded] = useState(false);
  const inputJson = (() => {
    try {
      return JSON.stringify(input, null, 2);
    } catch {
      return String(input);
    }
  })();

  if (!pending) {
    return (
      <button
        type="button"
        onClick={() => setIsExpanded(!isExpanded)}
        className="flex items-start gap-1.5 text-[10.5px] text-base-content/40 hover:text-base-content/60 w-full text-left group"
      >
        <span className="badge badge-ghost badge-xs flex-none">{tool} allowed</span>
        {isExpanded && (
          <pre className="flex-1 min-w-0 text-[11px] leading-relaxed text-base-content/80 bg-base-200/40 rounded-md p-3 whitespace-pre-wrap break-words">
            {inputJson}
          </pre>
        )}
        {!isExpanded && <span className="truncate flex-1">permission request</span>}
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
      <pre className="text-[11px] leading-relaxed text-base-content/80 max-h-[30vh] overflow-y-auto bg-base-200/40 rounded-md p-3 whitespace-pre-wrap break-words">
        {inputJson}
      </pre>
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
