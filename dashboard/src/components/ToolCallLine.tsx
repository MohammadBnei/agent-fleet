import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { parseToolCallSummary } from "../transcript";

// The sidecar's periodic {branch, files[]} push, rendered as one compact
// line — shared between mobile's exchange-zone feed and desktop's (when the
// "Changes" toggle is on), same file-change info desktop's dedicated
// CHANGES panel always shows regardless.
export function ToolCallLine({ entry }: { entry: TranscriptEntry }) {
  const summary = parseToolCallSummary(entry.text);
  const files = summary?.files ?? [];
  if (files.length === 0) return null;
  const added = files.reduce((n, f) => n + f.added, 0);
  const removed = files.reduce((n, f) => n + f.removed, 0);
  return (
    <div className="flex items-center gap-2 text-[11px] min-w-0">
      <span className="text-secondary flex-none">⏺</span>
      <span className="text-base-content/50 flex-none">files</span>
      <span className="text-base-content/70 flex-1 truncate min-w-0">
        {files.length} changed{summary?.branch ? ` · ${summary.branch}` : ""}
      </span>
      <span className="text-success flex-none">+{added}</span>
      {removed > 0 && <span className="text-warning flex-none">−{removed}</span>}
    </div>
  );
}
