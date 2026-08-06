import type { ToolCallPair, SdkToolUse, SdkToolResult } from "../transcript";
import { summarizeToolInput } from "../transcript";
import { JsonView } from "./JsonView";
import { Collapse } from "./Collapse";

// Read/Write/Edit's one-line result summary (line/word counts) already says
// everything the raw input JSON would — no separate detail view for them.
const SELF_DESCRIBING_TOOLS = new Set(["Read", "Write", "Edit"]);

function countLines(text: string): number {
  return text.length === 0 ? 0 : text.split("\n").length;
}

// Path-like input values (the common case — file_path) get tail-truncated
// instead of CSS end-truncated: in a narrow sidebar the filename at the end
// matters more than the leading directories. Full value stays in the title
// tooltip.
function shortValue(value: string): string {
  const idx = value.lastIndexOf("/");
  return idx === -1 || idx === value.length - 1 ? value : value.slice(idx + 1);
}

// Collapses call+result into one human-phrased trailing label instead of
// the CALL/OUTPUT JSON dump this used to show — the one-liner IS the
// summary now, not a preview of a duplicate detail view.
function summarizeResult(callInfo: SdkToolUse, resultInfo: SdkToolResult | null, isError: boolean): string {
  if (!resultInfo) return "";
  if (isError) return "failed";
  const tool = callInfo.tool;
  const content = resultInfo.content;
  if (tool === "Read" && typeof content === "string") return `${countLines(content)} lines`;
  if (tool === "Write") {
    const written = (callInfo.input as { content?: string } | undefined)?.content;
    return typeof written === "string" ? `${countLines(written)} lines written` : "written";
  }
  if (tool === "Edit") {
    const input = callInfo.input as { old_string?: string; new_string?: string } | undefined;
    const removed = typeof input?.old_string === "string" ? countLines(input.old_string) : 0;
    const added = typeof input?.new_string === "string" ? countLines(input.new_string) : 0;
    return `−${removed} +${added}`;
  }
  if (typeof content === "string") {
    const lines = countLines(content);
    return lines <= 1 ? "done" : `${lines} lines`;
  }
  if (Array.isArray(content)) return `${content.length} items`;
  return "done";
}

// Most calls need zero interaction — Read/Write/Edit are fully described by
// their one-liner, and a trivial/empty result has nothing further to show.
// Only errors and non-trivial output from everything else (Bash, Grep,
// Glob, ...) get a disclosure arrow.
function needsDetail(tool: string | undefined, isError: boolean, resultInfo: SdkToolResult | null): boolean {
  if (isError) return true;
  if (!resultInfo) return false;
  if (tool && SELF_DESCRIBING_TOOLS.has(tool)) return false;
  const content = resultInfo.content;
  if (typeof content === "string") return content.trim().length > 0;
  return content != null;
}

export function ToolCallItem({ pair }: { pair: ToolCallPair }) {
  const { callInfo, result, resultInfo } = pair;
  const isError = resultInfo?.isError ?? false;
  const tool = callInfo.tool ?? "tool";
  const fullValue = summarizeToolInput(callInfo.input);
  const resultLabel = summarizeResult(callInfo, resultInfo ?? null, isError);

  const line = (
    <div className="flex items-center gap-1.5 py-1 px-2 text-[11px]">
      {!result ? (
        <span className="loading loading-spinner loading-xs flex-none opacity-60" />
      ) : (
        <span className={`flex-none ${isError ? "text-error" : "text-success"}`}>{isError ? "✗" : "⏺"}</span>
      )}
      <span className="opacity-50 flex-none">{tool}</span>
      <span className="truncate flex-1 min-w-0" title={fullValue}>
        {shortValue(fullValue)}
      </span>
      {resultLabel && <span className={`flex-none ${isError ? "text-error" : "opacity-50"}`}>{resultLabel}</span>}
    </div>
  );

  if (!needsDetail(callInfo.tool, isError, resultInfo ?? null)) {
    return <div className="border border-base-content/10 bg-base-200/40 rounded-md">{line}</div>;
  }

  return (
    <Collapse summary={line} summaryClassName="p-0">
      <div className={`text-[10.5px] font-mono ${isError ? "text-error" : ""}`}>
        {typeof resultInfo?.content === "string" ? (
          <div className="whitespace-pre-wrap">{resultInfo.content}</div>
        ) : (
          <JsonView value={resultInfo?.content} />
        )}
      </div>
    </Collapse>
  );
}
