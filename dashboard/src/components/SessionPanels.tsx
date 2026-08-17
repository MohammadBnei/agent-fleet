
import type { Session } from "../gen/agentfleet/v1/core_pb";
import type { ToolCallSummary, TodoItem } from "../transcript";
import { imageTag } from "../imageTag";
import { TickBar, todoProgress } from "./TickBar";
import { ActionsMenu } from "./ActionsMenu";

// TODOS / CHANGES / E2E PREVIEW / SESSION. Desktop puts these in a fixed 266px
// right column, mobile behind "panels ▸" as a bottom sheet — one component so
// the two can't drift, which is how mobile ended up a weaker port the first
// time round.
//
// The drag-resizable, collapsible, fit-height Panel machinery this replaces
// (~120 lines and six localStorage keys) is gone: both mockups specify a fixed
// column, and nothing in the panels needed the extra height a drag could buy.

function PanelHeading({ title, extra }: { title: string; extra?: React.ReactNode }) {
  return (
    <div className="flex items-baseline gap-2 mb-2.5">
      <span className="text-2xs tracking-[0.12em] text-dim2">{title}</span>
      {extra}
    </div>
  );
}

export function TodosPanel({ todos, blocked }: { todos: TodoItem[]; blocked: boolean }) {
  return (
    <div>
      <PanelHeading
        title="TODOS"
        extra={todos.length > 0 && <span className="text-xs text-dim2 ml-auto">{todoProgress(todos)}</span>}
      />
      {todos.length === 0 ? (
        <div className="text-xs text-dim2">no todos yet</div>
      ) : (
        <>
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
        </>
      )}
    </div>
  );
}

export function ChangesPanel({ branch, changes }: { branch: string | null; changes: ToolCallSummary["files"] | null }) {
  return (
    <div className="min-w-0">
      <PanelHeading title="CHANGES" />
      {branch && <div className="text-xs text-dim2 mb-2 truncate">{branch}</div>}
      {!changes || changes.length === 0 ? (
        <div className="text-xs text-dim2">no changes yet</div>
      ) : (
        <div className="flex flex-col gap-1.5">
          {changes.map((c, i) => (
            <div key={i} className="flex gap-2 text-sm min-w-0">
              <span className="text-dim flex-1 min-w-0 truncate" title={c.path}>
                {c.path}
              </span>
              <span className="text-green-soft flex-none">+{c.added}</span>
              <span className={c.removed > 0 ? "text-minus flex-none" : "text-dim2 flex-none"}>−{c.removed}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
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
  onBypassClick,
  onRestart,
}: {
  session: Session;
  busy: boolean;
  busyKey: string | null;
  run: (action: () => Promise<unknown>, key: string) => void;
  onBypassClick: () => void;
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
        onBypassClick={onBypassClick}
        onRestart={onRestart}
      />
    </div>
  );
}
