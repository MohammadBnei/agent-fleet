import type { SpineItem } from "../transcript";

// The desktop rail: this session's decision history in one column, so "what
// have I already told this agent, and what is it waiting on now" is answerable
// without scrolling a thousand-line feed. docs/dashboard-spec.md §8 item 4 —
// a long session had no zoom-out and no jump-to-next-decision.
//
// Every item is a jump target. The feed tags its rows with the entry seq, so
// clicking scrolls to the real entry rather than approximating a position.

const TONE: Record<SpineItem["kind"], { edge: string; text: string; bg: string }> = {
  allow: { edge: "border-line", text: "text-dim", bg: "" },
  deny: { edge: "border-line", text: "text-dim", bg: "" },
  plan: { edge: "border-line", text: "text-dim", bg: "" },
  pending: { edge: "border-error", text: "text-error", bg: "bg-pink-wash" },
  alarm: { edge: "border-orange-line", text: "text-warning", bg: "" },
};

export function DecisionSpine({
  items,
  onJump,
  onNextDecision,
}: {
  items: SpineItem[];
  onJump: (seq: bigint) => void;
  onNextDecision: (() => void) | null;
}) {
  const open = items.filter((i) => i.kind === "pending").length;
  const alarms = items.filter((i) => i.kind === "alarm").length;
  const resolved = items.length - open - alarms;

  return (
    <div className="w-[186px] flex-none border-r border-line px-3 py-3.5 flex flex-col gap-3 overflow-y-auto">
      <div className="text-2xs tracking-[0.12em] text-dim2 flex-none">DECISION SPINE</div>

      {items.length === 0 && (
        <div className="text-xs text-dim2 leading-relaxed">
          No decisions yet — nothing has needed you in this session.
        </div>
      )}

      {items.map((item, i) => {
        const tone = TONE[item.kind];
        return (
          <button
            key={`${item.seq}-${i}`}
            type="button"
            onClick={() => onJump(item.seq)}
            title="Jump to this point in the feed"
            className={`text-left border-l-2 pl-2.5 py-0.5 cursor-pointer hover:bg-base-300/50 ${tone.edge} ${tone.bg}`}
          >
            <div className={`text-xs leading-[1.5] break-words ${tone.text}`}>{item.label}</div>
            {(item.detail || item.time) && (
              <div className={`text-xs break-words ${item.kind === "pending" ? "text-pink-dim" : "text-dim2"}`}>
                {item.detail ? `“${item.detail}”` : item.time}
              </div>
            )}
          </button>
        );
      })}

      <div className="mt-auto flex flex-col gap-2.5 flex-none pt-3">
        {onNextDecision && (
          <button
            type="button"
            onClick={onNextDecision}
            className="border border-acc-line py-1.5 text-center text-xs cursor-pointer hover:border-primary hover:text-primary"
          >
            ↓ next decision
          </button>
        )}
        <div className="text-xs text-dim2 leading-[1.6]">
          {resolved} resolved · {open} open
          {alarms > 0 && (
            <>
              <br />
              <span className="text-warning">
                {alarms} alarm{alarms === 1 ? "" : "s"}
              </span>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
