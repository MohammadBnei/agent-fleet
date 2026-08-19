import { useState } from "react";
import { diffLines, type Line } from "./ToolInputView";

// The mockups' diff block: tinted +/− rows on the --code surface, truncated to
// the first few lines with a "▸ N more lines" expander. This is the thing a
// human reads before clicking allow, on a phone as often as at a desk, so it
// shows the change and not a JSON blob.
//
// The differ itself is the existing line-level LCS in ToolInputView — this is
// presentation only.

// Context lines carry no information once the surrounding change is visible,
// and on a 390px screen they crowd out the +/− lines that matter. Ranking
// changed lines first is what makes a 4-line budget readable.
function rank(lines: Line[]): Line[] {
  const firstChange = lines.findIndex((l) => l.kind !== "context");
  if (firstChange === -1) return lines;
  return lines.slice(firstChange);
}

export function DiffLines({
  before,
  after,
  lines: given,
  hunk,
  maxLines = 4,
  compact = false,
}: {
  before?: string;
  after?: string;
  // Pre-built lines, for callers that already have them.
  lines?: Line[];
  // Optional "@@ normalise_stage @@" header row.
  hunk?: string;
  maxLines?: number;
  compact?: boolean;
}) {
  const [expanded, setExpanded] = useState(false);
  const all = given ?? rank(diffLines(before ?? "", after ?? ""));
  if (all.length === 0) return null;

  const shown = expanded ? all : all.slice(0, maxLines);
  const hidden = all.length - shown.length;
  const row = compact ? "px-[9px] py-[3px] text-xs" : "px-[11px] py-[3px] text-sm";

  return (
    <div className="border border-line bg-code overflow-x-auto py-1.5">
      {hunk && <div className={`${row} text-dim2 whitespace-pre`}>{hunk}</div>}
      {shown.map((line, idx) => (
        <div
          key={idx}
          className={`${row} whitespace-pre ${
            line.kind === "add"
              ? "text-green-soft bg-plus-bg"
              : line.kind === "remove"
                ? "text-minus bg-pink-wash"
                : "text-dim2"
          }`}
        >
          {line.kind === "add" ? "+" : line.kind === "remove" ? "−" : " "} {line.text || " "}
        </div>
      ))}
      {hidden > 0 && (
        <button
          type="button"
          onClick={() => setExpanded(true)}
          className={`${row} w-full text-left text-dim2 hover:text-dim cursor-pointer`}
        >
          ▸ {hidden} more {hidden === 1 ? "line" : "lines"}
        </button>
      )}
      {expanded && all.length > maxLines && (
        <button
          type="button"
          onClick={() => setExpanded(false)}
          className={`${row} w-full text-left text-dim2 hover:text-dim cursor-pointer`}
        >
          ▾ collapse
        </button>
      )}
    </div>
  );
}
