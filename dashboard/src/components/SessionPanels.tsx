import type { Task } from "../gen/agentfleet/v1/core_pb";
import type { GetE2eStatusResponse } from "../gen/agentfleet/v1/dashboard_pb";
import type { ToolCallSummary, TodoItem } from "../transcript";
import { TickBar, todoProgress } from "./TickBar";
import { NotchCard } from "./NotchCard";
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
export function E2ePanel({ e2e }: { e2e: GetE2eStatusResponse | null }) {
  if (!e2e || !e2e.status) return null;
  const running = e2e.podPhase === "Running";
  const state = e2e.appReady
    ? { text: "ready", dot: "bg-success", cls: "text-green-soft" }
    : running
      ? { text: "starting", dot: "bg-info", cls: "text-info" }
      : { text: e2e.podPhase.toLowerCase() || e2e.status, dot: "border border-dim2", cls: "text-dim2" };

  const ingredients = [...e2e.tools, ...e2e.services];

  return (
    <NotchCard label="E2E PREVIEW" className="px-3 pt-3.5 pb-3 min-w-0">
      <div className="flex items-center gap-2 mb-2 min-w-0">
        <span className={`w-1.5 h-1.5 rounded-full flex-none ${state.dot}`} />
        <span className={`text-sm flex-none ${state.cls}`}>{state.text}</span>
        {e2e.restarts > 0 && (
          <span className="text-xs text-warning flex-none">
            · {e2e.restarts} restart{e2e.restarts === 1 ? "" : "s"}
          </span>
        )}
        {e2e.startCmdOverridden && (
          <span
            title="A human-approved override is running for this task only; the repo's profile is unchanged."
            className="ml-auto flex-none text-2xs text-warning border border-orange-line px-1"
          >
            overridden
          </span>
        )}
      </div>

      {e2e.previewUrl && e2e.appReady && (
        <a
          href={e2e.previewUrl}
          target="_blank"
          rel="noreferrer"
          title={e2e.previewUrl}
          className="block text-sm text-primary hover:underline truncate min-w-0"
        >
          {e2e.previewUrl}
        </a>
      )}

      {running && !e2e.appReady && (
        <div className="text-xs text-dim2 leading-snug min-w-0">
          Nothing on the app port yet — installing, or the command never binds{" "}
          <span className="text-dim">0.0.0.0:$PORT</span>.
        </div>
      )}

      <div className="text-xs text-dim2 mt-2 leading-[1.6] min-w-0">
        profile: {e2e.profileName || "none"}
        {e2e.startCmd && (
          <>
            <br />
            {/* overflow-wrap:anywhere, not break-all: `anywhere` still breaks a
                single unbreakable token so the box can shrink, but prefers
                whitespace — break-all produced "bunx prisma mig/rate deploy". */}
            <span className="text-dim [overflow-wrap:anywhere]">{e2e.startCmd}</span>
          </>
        )}
      </div>

      {ingredients.length > 0 && (
        <div className="flex flex-wrap gap-1 mt-2 min-w-0">
          {ingredients.map((i) => (
            <span key={i} className="text-2xs border border-line text-dim2 px-1">
              {i}
            </span>
          ))}
        </div>
      )}
    </NotchCard>
  );
}

export function SessionPanel({
  task,
  busy,
  run,
  previewUrl,
  isThotTask,
  onBypassClick,
}: {
  task: Task;
  busy: boolean;
  run: (action: () => Promise<unknown>, key: string) => void;
  previewUrl: string | null;
  isThotTask: boolean;
  onBypassClick: () => void;
}) {
  return (
    <div>
      <PanelHeading title="SESSION" />
      <div className="text-xs text-dim2 leading-[1.6] mb-2.5">
        mode <span className="text-text2">{task.permissionMode || "default"}</span>
        {task.retryCount > 0 && ` · attempt ${task.retryCount + 1}`}
        {task.podPhase && ` · pod ${task.podPhase.replace("POD_PHASE_", "")}`}
      </div>
      <ActionsMenu
        taskId={task.id}
        busy={busy}
        run={run}
        previewUrl={previewUrl}
        isThotTask={isThotTask}
        status={task.status}
        currentMode={task.permissionMode}
        podPhase={task.podPhase}
        onBypassClick={onBypassClick}
      />
    </div>
  );
}
