import { Modal } from "./Modal";
import { ToolCallDetail } from "./ToolCallItem";
import { entryTime } from "../transcript";
import { Markdown } from "./Markdown";
import type { ToolCallPair, SubagentRun } from "../transcript";

// The full story behind one row, for the two places that only ever had room
// for a truncated one: an AGENTS row (whose description and returned summary
// were single truncated lines with the rest in a `title` tooltip — invisible on
// a touch screen, which has no hover) and a tool call (whose inline expander
// opened inside a 390px column).
//
// One shell, two payloads, rather than two modals that drift.

function Row({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt className="text-2xs text-dim2 tracking-[0.08em]">{label}</dt>
      <dd className="text-sm text-base-content min-w-0 break-words">{value}</dd>
    </>
  );
}

function Heading({ kind, title, tone }: { kind: string; title: string; tone: string }) {
  return (
    <div className="flex flex-col gap-1.5 flex-none">
      <span className="text-2xs tracking-[0.12em] text-dim2">{kind}</span>
      {/* break-words, never truncate: being untruncated is the entire reason
          this modal exists. */}
      <span className={`text-base font-semibold break-words ${tone}`}>{title}</span>
    </div>
  );
}

const STATUS_TONE: Record<SubagentRun["status"], string> = {
  running: "text-base-content",
  completed: "text-success",
  failed: "text-error",
};
const STATUS_GLYPH: Record<SubagentRun["status"], string> = {
  running: "▸",
  completed: "✓",
  failed: "✗",
};

export function AgentDetailModal({ run, onClose }: { run: SubagentRun | null; onClose: () => void }) {
  if (!run) return null;
  return (
    <Modal open onClose={onClose} boxClassName="max-w-2xl max-h-[85vh] overflow-hidden flex flex-col">
      <div className="flex flex-col gap-4 overflow-y-auto min-h-0">
        <Heading
          kind="SUBAGENT"
          tone={STATUS_TONE[run.status]}
          title={`${STATUS_GLYPH[run.status]} ${run.subagentType}`}
        />
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 min-w-0">
          <Row label="STATUS" value={run.status} />
          {run.description && <Row label="TASK" value={run.description} />}
        </dl>
        {/* One agent's instructions to another. Two things it needs and did not
            have: markdown rendering — this is prose an agent WROTE, with
            headings, lists and code in it, and as a flat string it read as a
            wall — and visible attribution, because without it the text is
            indistinguishable from something the human typed. The rule and the
            arrow say who is talking to whom. */}
        {run.prompt && (
          <div className="flex flex-col gap-2 min-w-0">
            <div className="flex items-center gap-2">
              <span className="text-2xs tracking-[0.08em] text-dim2 whitespace-nowrap">
                this session's agent → {run.subagentType}
              </span>
              <span className="flex-1 h-px bg-line2" />
            </div>
            {/* Left rule + inset, the visual grammar for a quotation: this is
                relayed speech, not the console addressing you. */}
            <div className="border-l-2 border-primary/40 pl-3 min-w-0">
              <Markdown text={run.prompt} />
            </div>
          </div>
        )}
        {/* The answer. It was collected and then rendered as one truncated line,
            so "what did it actually find" was unanswerable from the console —
            which is most of the value of having spawned the subagent at all. */}
        {run.summary ? (
          <div className="flex flex-col gap-1.5 min-w-0">
            <span className="text-2xs tracking-[0.08em] text-dim2">RETURNED</span>
            <div className="text-sm leading-[1.6] whitespace-pre-wrap break-words rounded-md bg-base-200/40 p-3">
              {run.summary}
            </div>
          </div>
        ) : (
          <div className="text-sm text-dim2">
            {run.status === "running" ? "Still running — nothing returned yet." : "No summary was reported."}
          </div>
        )}
      </div>
    </Modal>
  );
}

export function ToolCallDetailModal({ pair, onClose }: { pair: ToolCallPair | null; onClose: () => void }) {
  if (!pair) return null;
  const failed = pair.resultInfo?.isError === true;
  const when = entryTime(pair.call);
  return (
    <Modal open onClose={onClose} boxClassName="max-w-3xl max-h-[85vh] overflow-hidden flex flex-col">
      <div className="flex flex-col gap-4 overflow-y-auto min-h-0">
        <Heading
          kind="TOOL CALL"
          tone={failed ? "text-error" : "text-base-content"}
          title={pair.callInfo.tool ?? "tool"}
        />
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 min-w-0">
          <Row label="STATUS" value={failed ? "failed" : pair.result ? "ok" : "running"} />
          {when && <Row label="STARTED" value={when} />}
        </dl>
        {/* Same renderer the inline expander used, so an Edit is still a real
            diff and a Bash is still its command — just with room to read it. */}
        <ToolCallDetail pair={pair} />
      </div>
    </Modal>
  );
}
