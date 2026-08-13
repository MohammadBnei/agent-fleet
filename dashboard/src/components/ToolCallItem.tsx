import { memo } from "react";
import type { ToolCallPair, SdkToolUse, SdkToolResult } from "../transcript";
import { summarizeToolInput } from "../transcript";
import { JsonView } from "./JsonView";
import { ToolInputView } from "./ToolInputView";
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

// Most calls need zero interaction — Read's one-liner says everything, and
// a trivial/empty result has nothing further to show. Errors, non-trivial
// output, and anything whose *input* is worth reading (an Edit's diff, a
// Write's content, a Bash command) get a disclosure arrow.
function needsDetail(tool: string | undefined, isError: boolean, resultInfo: SdkToolResult | null, input: unknown): boolean {
  if (isError) return true;
  if (hasRenderableInput(tool, input)) return true;
  if (!resultInfo) return false;
  if (tool && SELF_DESCRIBING_TOOLS.has(tool)) return false;
  const content = resultInfo.content;
  if (typeof content === "string") return content.trim().length > 0;
  return content != null;
}

// Whether expanding would reveal something the one-line summary can't say.
// "Edit changed 3 lines for 2" is not the same as seeing which lines.
function hasRenderableInput(tool: string | undefined, input: unknown): boolean {
  if (!tool || !input || typeof input !== "object") return false;
  const obj = input as Record<string, unknown>;
  if (tool === "Edit") return typeof obj.old_string === "string" && typeof obj.new_string === "string";
  if (tool === "Write") return typeof obj.content === "string";
  if (tool === "Bash") return typeof obj.command === "string";
  return false;
}

// Just the expanded body — the call's own input and its result — with no
// summary line or collapse wrapper. SessionFeed's grouped tool block renders
// its own dense summary rows (the mockups' 70px-name layout) and needs only
// this when one is opened, rather than a second nested disclosure.
export function ToolCallDetail({ pair }: { pair: ToolCallPair }) {
  const { callInfo, resultInfo } = pair;
  const isError = resultInfo?.isError ?? false;
  return (
    <div className="flex flex-col gap-1.5">
      {hasRenderableInput(callInfo.tool, callInfo.input) && (
        <ToolInputView tool={callInfo.tool ?? "tool"} input={callInfo.input} />
      )}
      {resultInfo ? (
        <div className={`text-xs font-mono ${isError ? "text-error" : "text-dim"}`}>
          {typeof resultInfo.content === "string" ? (
            <div className="whitespace-pre-wrap break-words">{resultInfo.content}</div>
          ) : (
            <JsonView value={resultInfo.content} />
          )}
        </div>
      ) : (
        <div className="text-xs text-dim2">still running — no output yet.</div>
      )}
    </div>
  );
}

// Memoized to prevent re-rendering when the pair hasn't changed.
export const ToolCallItem = memo(function ToolCallItem({ pair }: { pair: ToolCallPair }) {
  const { callInfo, result, resultInfo } = pair;
  const isError = resultInfo?.isError ?? false;
  const tool = callInfo.tool ?? "tool";
  const fullValue = summarizeToolInput(callInfo.input);
  const resultLabel = summarizeResult(callInfo, resultInfo ?? null, isError);

  const line = (
    <div className="flex items-center gap-1.5 py-1 px-2 text-xs">
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

  if (!needsDetail(callInfo.tool, isError, resultInfo ?? null, callInfo.input)) {
    return <div className="border border-base-content/10 bg-base-200/40 rounded-md">{line}</div>;
  }

  return (
    <Collapse summary={line} summaryClassName="p-0">
      <div className="flex flex-col gap-1.5 pb-1">
        {/* The call's own input — previously unreachable from the UI at
            any point, so "what did it actually edit?" had no answer
            unless the call happened to require permission. */}
        {hasRenderableInput(callInfo.tool, callInfo.input) && <ToolInputView tool={tool} input={callInfo.input} />}
        {resultInfo && (
          <div className={`text-2xs font-mono ${isError ? "text-error" : ""}`}>
            {typeof resultInfo.content === "string" ? (
              <div className="whitespace-pre-wrap">{resultInfo.content}</div>
            ) : (
              <JsonView value={resultInfo.content} />
            )}
          </div>
        )}
      </div>
    </Collapse>
  );
});
