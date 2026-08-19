
import type { Session } from "../gen/agentfleet/v1/core_pb";
import type { ToolCallSummary, TodoItem, SubagentRun } from "../transcript";
import { imageTag } from "../imageTag";
import { TickBar, todoProgress } from "./TickBar";
import { ActionsMenu } from "./ActionsMenu";

// TODOS / CHANGES / AGENTS / SESSION. Desktop puts these in a fixed 266px
// right column, mobile behind "panels ▸" as a bottom sheet — one component so
// the two can't drift, which is how mobile ended up a weaker port the first
// time round.
//
// The drag-resizable, collapsible, fit-height Panel machinery this replaces
// (~120 lines and six localStorage keys) is gone: both mockups specify a fixed
// column, and nothing in the panels needed the extra height a drag could buy.
//
// The first three render NOTHING when empty rather than a heading over a "no
// todos yet" line. Three such placeholders is what a fresh session showed —
// a column of headings asserting the absence of things nobody had asked about
// yet, and on mobile a bottom sheet worth opening to read them. SESSION is the
// exception: it carries the facts and the action bar, so it is never empty.
// panelsEmpty() below is what lets mobile hide its opener on the same rule.

function PanelHeading({ title, extra }: { title: string; extra?: React.ReactNode }) {
  return (
    <div className="flex items-baseline gap-2 mb-2.5">
      <span className="text-2xs tracking-[0.12em] text-dim2">{title}</span>
      {extra}
    </div>
  );
}

export function TodosPanel({ todos, blocked }: { todos: TodoItem[]; blocked: boolean }) {
  if (todos.length === 0) return null;
  return (
    <div>
      <PanelHeading title="TODOS" extra={<span className="text-xs text-dim2 ml-auto">{todoProgress(todos)}</span>} />
      <TickBar todos={todos} blocked={blocked} className="mb-3" />
      <div className="flex flex-col gap-1.5">
        {todos.map((t, i) => (
          <div
            key={i}
            className={`text-sm leading-[1.5] ${
              t.status === "in_progress" ? (blocked ? "text-error" : "text-base-content") : "text-dim2"
            }`}
          >
            {t.status === "completed" ? "✓ " : t.status === "in_progress" ? "▸ " : "· "}
            {t.status === "in_progress" ? t.activeForm : t.content}
            {t.status === "in_progress" && blocked && (
              <>
                <br />
                <span className="text-pink-dim">blocked on you</span>
              </>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

// Each row opens the file's diff (see FileDiffModal): the sidecar's numstat
// telemetry gives a line count, and a line count is the one thing about a
// change nobody actually wants to know on its own.
export function ChangesPanel({
  branch,
  changes,
  onOpenFile,
}: {
  branch: string | null;
  changes: ToolCallSummary["files"] | null;
  onOpenFile: (path: string) => void;
}) {
  if (!changes || changes.length === 0) return null;
  return (
    <div className="min-w-0">
      <PanelHeading title="CHANGES" />
      {branch && <div className="text-xs text-dim2 mb-2 truncate">{branch}</div>}
      <div className="flex flex-col gap-1.5">
        {changes.map((c, i) => (
          <button
            key={i}
            type="button"
            onClick={() => onOpenFile(c.path)}
            className="flex gap-2 text-sm min-w-0 text-left cursor-pointer hover:text-primary"
            title={`${c.path} — open diff`}
          >
            <span className="text-dim flex-1 min-w-0 truncate">{c.path}</span>
            <span className="text-green-soft flex-none">+{c.added}</span>
            <span className={c.removed > 0 ? "text-minus flex-none" : "text-dim2 flex-none"}>−{c.removed}</span>
          </button>
        ))}
      </div>
    </div>
  );
}

// The subagents this session spawned. They used to be six kinds of raw JSON
// blob stuttering through the feed — one task_started carrying the subagent's
// entire prompt, then a task_progress per poll, a task_updated, a
// task_notification, and a background_tasks_changed re-listing every running
// task each time. All of that is one row here, and none of it is in the feed.
//
// Tasks and subagents only. Skills and Bash stay in the feed where their tool
// call already is — this panel exists to remove a duplicate, not to add one.
export function AgentsPanel({ runs }: { runs: SubagentRun[] }) {
  if (runs.length === 0) return null;
  const running = runs.filter((r) => r.status === "running").length;
  return (
    <div className="min-w-0">
      <PanelHeading
        title="AGENTS"
        extra={
          <span className="text-xs text-dim2 ml-auto">
            {running > 0 ? `${running} running` : `${runs.length} done`}
          </span>
        }
      />
      <div className="flex flex-col gap-1.5">
        {runs.map((r) => (
          <div key={r.toolUseId} className="min-w-0">
            <div
              className={`text-sm leading-[1.5] truncate ${
                r.status === "failed" ? "text-error" : r.status === "running" ? "text-base-content" : "text-dim2"
              }`}
              title={r.description || r.subagentType}
            >
              {r.status === "completed" ? "✓ " : r.status === "failed" ? "✗ " : "▸ "}
              {r.subagentType}
            </div>
            {r.description && (
              <div className="text-xs text-dim2 truncate" title={r.description}>
                {r.description}
              </div>
            )}
            {/* What it came back with. The Agent call's own tool_result is
                filtered out of the feed along with the call, so this line is
                the only trace of the subagent's answer left in the console —
                collecting the summary and rendering nowhere left "what did
                it find" unanswerable. Full text in the tooltip. */}
            {r.status !== "running" && r.summary && (
              <div className="text-xs text-dim2 truncate" title={r.summary}>
                {r.summary}
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}

// Whether the three conditional panels would all render null. Mobile hides the
// "panels" opener on this rather than each caller re-deriving the same three
// emptiness checks the panels already make internally — a button that opens an
// empty sheet is worse than no button.
export function panelsEmpty(todos: TodoItem[], changes: ToolCallSummary["files"] | null, runs: SubagentRun[]) {
  return todos.length === 0 && (!changes || changes.length === 0) && runs.length === 0;
}

// min-w-0 on the root and on every shrinkable child is load-bearing: `truncate`
// implies white-space:nowrap, which makes the preview URL's min-content width
// the whole URL. Without it a flex item refuses to shrink below that, and this
// card silently widened the mobile column to 437px on a 390px viewport — caught
// by measuring rects in Playwright, invisible to tsc and to lint.

// One `label  value` row of the SESSION panel. min-w-0 + truncate so a long
// image tag shortens instead of widening the column (see the note above).
function Fact({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt className="text-dim2">{label}</dt>
      <dd className="text-text2 min-w-0 truncate" title={value}>
        {value}
      </dd>
    </>
  );
}

export function SessionPanel({
  session,
  busy,
  busyKey,
  run,
  onAutoClick,
  onRestart,
}: {
  session: Session;
  busy: boolean;
  busyKey: string | null;
  run: (action: () => Promise<unknown>, key: string) => void;
  onAutoClick: () => void;
  onRestart?: () => void;
}) {
  return (
    <div>
      <PanelHeading title="SESSION" />
      {/* Label/value rows rather than one ` · `-joined line: these are five
          unrelated facts, and run together they read as a sentence nobody
          finishes. The label stays dim, the value is what the eye lands on. */}
      <dl className="text-xs leading-[1.6] mb-2.5 grid grid-cols-[auto_1fr] gap-x-2.5 gap-y-1 min-w-0">
        <Fact label="model" value={session.model || "default"} />
        <Fact label="mode" value={session.permissionMode || "default"} />
        {session.podPhase && <Fact label="pod" value={session.podPhase.replace("POD_PHASE_", "").toLowerCase()} />}
        {/* The build this session's pod is actually on — recorded at dispatch,
            so it stays true after the fleet is upgraded underneath it. Absent
            until the session has had a pod. */}
        {session.workerImage && <Fact label="worker" value={imageTag(session.workerImage)} />}
        {session.sidecarImage && <Fact label="sidecar" value={imageTag(session.sidecarImage)} />}
      </dl>
      <ActionsMenu
        sessionId={session.id}
        busy={busy}
        busyKey={busyKey}
        run={run}
        sweptAt={session.sweptAt}
        archivedAt={session.archivedAt}
        currentMode={session.permissionMode}
        podPhase={session.podPhase}
        onAutoClick={onAutoClick}
        onRestart={onRestart}
      />
    </div>
  );
}
