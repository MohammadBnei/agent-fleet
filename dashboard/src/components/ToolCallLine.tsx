import { memo } from "react";
import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { parseToolCallSummary } from "../transcript";

// The sidecar's periodic {branch, files[]} push, rendered as one compact
// line — shared between mobile's exchange-zone feed and desktop's (when the
// "Changes" toggle is on), same file-change info desktop's dedicated
// CHANGES panel always shows regardless.
// Memoized to prevent re-rendering when the entry hasn't changed.
export const ToolCallLine = memo(function ToolCallLine({ entry }: { entry: TranscriptEntry }) {
  const summary = parseToolCallSummary(entry.text);
  const files = summary?.files ?? [];
  if (files.length === 0) return null;
  const added = files.reduce((n, f) => n + f.added, 0);
  const removed = files.reduce((n, f) => n + f.removed, 0);
  return (
    <div className="flex items-center gap-2.5 min-w-0">
      <span className="text-[11px] text-dim2 whitespace-nowrap">
        {files.length} file{files.length === 1 ? "" : "s"} changed
      </span>
      <span className="flex-1 h-px bg-line3" />
      <span className="text-[11px] text-dim2 truncate min-w-0">{summary?.branch ?? ""}</span>
      <span className="text-[11px] text-green-soft flex-none">+{added}</span>
      {removed > 0 && <span className="text-[11px] text-minus flex-none">−{removed}</span>}
    </div>
  );
});
