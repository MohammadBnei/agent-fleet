import type { ToolCallPair } from "../transcript";
import { summarizeToolInput } from "../transcript";
import { JsonView } from "./JsonView";

// One collapsible call+output unit, shared between desktop's right-panel
// TOOL CALLS section and mobile's inline feed — native <details>/<summary>
// with daisyUI's collapse classes, defaults closed, no JS state needed.
// CALL and OUTPUT are explicitly labeled sub-sections (not just two boxes
// next to each other) since "is this the input or the result" was the
// actual complaint the generic per-type bubbles didn't address.
export function ToolCallItem({ pair }: { pair: ToolCallPair }) {
  const { callInfo, result, resultInfo } = pair;
  const isError = resultInfo?.isError ?? false;
  const statusBadge = !result ? "badge-info" : isError ? "badge-error" : "badge-ghost";

  return (
    <details className="collapse collapse-arrow border border-base-content/10 bg-base-200/40 rounded-md text-[11px]">
      <summary className="collapse-title min-h-0 py-2 pl-3 pr-8 flex items-center gap-2 cursor-pointer">
        <span className={`badge badge-xs ${statusBadge} flex-none`}>{callInfo.tool ?? "tool"}</span>
        <span className="opacity-50 truncate">{summarizeToolInput(callInfo.input)}</span>
        {!result && <span className="loading loading-spinner loading-xs flex-none opacity-60" />}
      </summary>
      <div className="collapse-content flex flex-col gap-2 pl-3 pr-3 pb-3">
        <div>
          <div className="text-[9px] tracking-wide opacity-40 mb-1">CALL</div>
          <div className="text-[10.5px] font-mono">
            <JsonView value={callInfo.input} />
          </div>
        </div>
        <div>
          <div className="text-[9px] tracking-wide opacity-40 mb-1">OUTPUT</div>
          {result ? (
            <div className={`text-[10.5px] font-mono ${isError ? "text-error" : ""}`}>
              {typeof resultInfo?.content === "string" ? (
                <div className="whitespace-pre-wrap">{resultInfo.content}</div>
              ) : (
                <JsonView value={resultInfo?.content} />
              )}
            </div>
          ) : (
            <div className="text-[10.5px] opacity-40">running…</div>
          )}
        </div>
      </div>
    </details>
  );
}
