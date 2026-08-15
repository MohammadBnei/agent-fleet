import type { Session } from "../gen/agentfleet/v1/core_pb";
import { client } from "../connectClient";
import { sessionLabel } from "../sessionLabel";
import type { ListSummary } from "../transcript";
import { TickBar, todoProgress } from "../components/TickBar";
import { Collapse } from "../components/Collapse";
import { DecisionInline } from "../components/DecisionInline";
import { NotchCard } from "../components/NotchCard";

// A session with a live pod. Replaces ACTIVE_STATUSES, which named the three
// task statuses that meant "dispatched" — all gone with the enum
// (docs/adr/0048). Live state is derived from the row on every read, so this
// cannot drift from the transcript the way a cached status could.
const LIVE_STATES = new Set(["working", "blocked", "idle", "stalled", "unknown"]);
export const ACTIVE_STATES = LIVE_STATES;

// Shared section-membership split — used by both list views so the two never
// disagree about which bucket a session lands in.
//
// Every session must land in at least one bucket. TaskList.tsx carried a
// comment from whoever discovered that the hard way: a row matching no bucket
// vanishes from both lists, which is the one place a human is supposed to see
// it. bucketTasks.test.ts guards exactly this, and it is why `quiet` is
// defined as "everything not already claimed" rather than by its own
// predicate.
export function bucketTasks(tasks: Session[], needsYouIds: Set<string>) {
  const out = {
    needsYou: [] as Session[],
    archived: [] as Session[],
    swept: [] as Session[],
    finished: [] as Session[],
    stalled: [] as Session[],
    working: [] as Session[],
    quiet: [] as Session[],
  };

  // One pass with explicit precedence, rather than seven independent filters
  // that each re-derive which of the others they need to exclude.
  //
  // That shape had already drifted: `swept` excluded nothing, so a swept
  // session rendered under both "idle" and "swept", and a stalled session that
  // had been archived showed up twice as well. Each new bucket needed a new
  // exclusion added to every filter below it, and a missed one is a duplicate
  // row rather than an error.
  //
  // Assigning each session exactly once makes the buckets a partition by
  // construction: every session lands in one and only one, which is what the
  // list needs and what bucketTasks.test.ts asserts.
  for (const t of tasks) {
    // 1. A pending decision outranks everything: the session is stalled until
    //    someone clicks, and surfacing that is the product's whole job.
    //    pendingDecisions is a COUNT, not the old awaiting_human boolean —
    //    parallel tool calls each get their own permission, and answering one
    //    must not report the rest as resolved.
    //    Keyed on liveState, NOT on the raw pendingDecisions count. The
    //    server derives "blocked" as live-AND-has-pending, so the count alone
    //    disagrees with it by construction for a session whose pod is gone:
    //    that one would pin itself to the top of the list forever, offering
    //    allow/deny buttons that reach nothing. Live and pending always
    //    derives to "blocked", so nothing is lost by dropping the count.
    if (t.archivedAt === undefined && (t.liveState === "blocked" || needsYouIds.has(t.id))) {
      out.needsYou.push(t);
      continue;
    }
    // 2. The human already said they were finished with this one. Their
    //    statement outranks anything computed about it.
    if (t.archivedAt !== undefined) {
      out.archived.push(t);
      continue;
    }
    // 3. Disk reclaimed by the retention GC: readable, not resumable.
    //    Rendered without a Warm button — offering one would be an action
    //    that cannot succeed.
    if (t.sweptAt !== undefined) {
      out.swept.push(t);
      continue;
    }
    // 4. Finished-while-you-were-away: `done` liveness means the session
    //    completed and nobody has opened it since — opening marks it seen,
    //    which is what turns this back into idle. A crashed pod is always
    //    news, and with no `failed` status left it is the only thing that
    //    says so.
    if (t.liveState === "done" || t.podPhase === "POD_PHASE_CRASHED") {
      out.finished.push(t);
      continue;
    }
    if (t.liveState === "stalled") {
      out.stalled.push(t);
      continue;
    }
    if (t.liveState === "working") {
      out.working.push(t);
      continue;
    }
    // 5. Everything left: seen, idle, or dormant. The collapsed tail, and the
    //    catch-all that guarantees nothing falls through.
    out.quiet.push(t);
  }

  return out;
}

// prBadge is gone with the pr_url column (docs/adr/0048), whose only writer
// never actually passed it — so this badge has been rendering `null` for every
// session in the fleet's history.
//
// The PR is discoverable without storing it: the agent names its own branch,
// and a GitHub search on that branch cannot go stale the way a column can.

// Mirrors core/internal/tasks/store.go's IsPodPhaseLive — the client-side half
// of the same "does this session have a live pod right now" check the
// Warm/Discuss handlers use, so ActionsMenu can show Warm vs Stop without a
// round trip.
export function isPodPhaseLive(phase?: string): boolean {
  return phase === "POD_PHASE_PROVISIONING" || phase === "POD_PHASE_CREATED" || phase === "POD_PHASE_SCHEDULED" || phase === "POD_PHASE_RUNNING";
}

// Matches core's own reclaim threshold (tasks.Store.ClaimNextTask reclaims a
// claimed/running task once its heartbeat is this stale) — the dashboard's
// "stuck" signal should fire at exactly the point a human can no longer
// distinguish "about to be reclaimed" from "silently wedged forever" (see the
// incident where a task sat claimed with no pod for 20+ minutes and nothing in
// the UI showed it).

// ONE badge per session, chosen by precedence.
//
// Status, pod phase, liveness and staleness were four independently rendered
// badges of near-identical weight, and they overlap hard: a `claimed`/`running`
// status means "there is a live pod", which is what the pod badge already says;
// `unknown` liveness renders "STARTING", which is what
// `PROVISIONING`/`SCHEDULED` already says. Four badges that mostly restate each
// other read as noise, and the one that matters — a session waiting on a human —
// does not stand out from the three that don't.
//
// The order below is "what does a human need to know first", not the order the
// data happens to arrive in. Everything demoted out of the label survives in the
// tooltip, so nothing is lost, only ranked.
//
// Unchanged by the console rewrite, deliberately: docs/dashboard-spec.md §8
// item 1 asks for the ranking to be kept and given more visual weight, which is
// the callers' job, not this function's.
export function sessionBadge(task: Session): { label: string; className: string; title?: string } | null {
  const stale = staleBadge(task);
  const phase = task.podPhase?.replace("POD_PHASE_", "");

  // 1. Needs a human. Always wins: nothing else about this session matters
  //    until someone clicks, and this is the product's whole job.
  if (task.liveState === "blocked") {
    return { label: "NEEDS YOU", className: "text-error border-pink-line bg-pink-chip", title: "waiting on your decision" };
  }
  // 2. Something is wrong, in order of how wrong.
  if (phase === "CRASHED") {
    return { label: "CRASHED", className: "text-error border-pink-line bg-pink-chip", title: task.podMessage || task.lastError || undefined };
  }
  if (stale) return stale;
  if (task.liveState === "stalled") {
    return { label: "STALLED", className: "text-warning border-orange-line bg-orange-bg", title: "no response since the last thing sent to the agent" };
  }
  // 3. Finished while nobody was looking — the "welcome back" state.
  if (task.liveState === "done") {
    return { label: "DONE", className: "text-success border-green-line bg-green-bg", title: "finished while you weren't looking — opening it marks it seen" };
  }
  // 4. Healthy and in motion. PROVISIONING keeps its sub-step inline: the
  //    difference between "cloning repo" and no event for 20 minutes is the
  //    exact gap that made a real incident invisible.
  if (phase === "PROVISIONING") {
    return {
      label: task.podMessage ? `PROVISIONING: ${task.podMessage}` : "PROVISIONING",
      className: "text-info border-info/45 bg-info/10 animate-pulse",
    };
  }
  if (task.liveState === "working") {
    return { label: "WORKING", className: "text-info border-info/45 bg-info/10", title: `pod ${phase?.toLowerCase() ?? "live"}` };
  }
  if (task.liveState === "unknown") {
    return { label: "STARTING", className: "text-info border-info/45 bg-info/10", title: "pod is up, the agent hasn't spoken yet" };
  }
  if (task.liveState === "idle") {
    return { label: "IDLE", className: "text-dim2 border-line bg-transparent", title: "pod live, nothing in flight" };
  }
  // 5. No live pod. The five workflow statuses that used to be rendered here
  //    (PROPOSED, QUEUED, FAILED, FAILED (final), CANCELLED, DONE) are gone
  //    with the enum (docs/adr/0048): there is no queue to be QUEUED in, no
  //    reclaim to be FAILED (final) from, and a proposal is a row in a
  //    different table with its own view. What is left is genuinely about the
  //    session rather than a workflow position.
  if (task.archivedAt) {
    return { label: "ARCHIVED", className: "text-dim2 border-line bg-transparent", title: "you marked this finished" };
  }
  if (task.sweptAt) {
    return {
      label: "SWEPT",
      className: "text-dim2 border-line bg-transparent",
      title: "working directory reclaimed by the retention GC — readable, not resumable",
    };
  }
  if (task.lastError) {
    return { label: "ERROR", className: "text-warning border-orange-line bg-orange-bg", title: task.lastError };
  }
  return null;
}

// staleBadge is gone with heartbeat_at. It fired when a heartbeat was 10
// minutes stale, mirroring the reclaim threshold — but there is no heartbeat
// and no reclaim now. The equivalent signal is CRASHED, written by the
// reconcile loop when Kubernetes says the pod is gone (docs/adr/0048).
function staleBadge(_: Session): { label: string; className: string; title?: string } | null {
  return null;
}

// heartbeatLabel is gone with heartbeat_at (docs/adr/0048). last_active_at
// is the honest replacement: it says when something actually happened,
// where a heartbeat only said a timer was still running.

// How long a session has been blocked — the notch label's "· 4m". This is the
// product's real cost (a blocked session is stalled until a human clicks), so
// it's stated on the card rather than left to be inferred from a heartbeat.
export function blockedForLabel(task: Session): string | null {
  if (!task.lastActiveAt) return null;
  const ms = Date.now() - new Date(task.lastActiveAt).getTime();
  if (Number.isNaN(ms) || ms < 0) return null;
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m`;
  return `${Math.floor(minutes / 60)}h`;
}

export function SectionHeading({
  title,
  tone = "dim",
  note,
  className = "",
}: {
  title: string;
  tone?: "pink" | "green" | "dim";
  note?: string;
  className?: string;
}) {
  const color = tone === "pink" ? "text-error" : tone === "green" ? "text-success" : "text-dim2";
  return (
    <div className={`flex items-center gap-2.5 ${className}`}>
      <span className={`text-xs tracking-[0.14em] ${color} whitespace-nowrap`}>{title}</span>
      <span className={`flex-1 h-px ${tone === "pink" ? "bg-line" : "bg-line2"}`} />
      {note && <span className="text-xs text-dim2 whitespace-nowrap">{note}</span>}
    </div>
  );
}

function DeleteButton({ onDelete }: { onDelete: () => void }) {
  return (
    <button
      type="button"
      onClick={onDelete}
      title="Delete this session"
      aria-label="Delete this session"
      className="absolute top-1.5 right-1.5 w-5 h-5 flex items-center justify-center text-dim2 hover:text-error hover:bg-pink-chip text-xs"
    >
      ✕
    </button>
  );
}

// A blocked session, with its actual pending decision rendered inline. Before
// the rewrite this card said only "a decision is waiting" and made you open the
// session to find out which — the latency that costs is the product's whole
// point (docs/dashboard-spec.md §8 item 3).
function NeedsYouCard({
  task,
  summary,
  onSelect,
  onDelete,
  reload,
}: {
  task: Session;
  summary?: ListSummary;
  onSelect: () => void;
  onDelete: () => void;
  reload: () => void;
}) {
  const todos = summary?.todos ?? [];
  const blockedFor = blockedForLabel(task);
  const isProposal = false;

  return (
    <div className="relative">
      <NotchCard
        label={isProposal ? "◉ PROPOSED — NEEDS APPROVAL" : `◉ BLOCKED${blockedFor ? ` · ${blockedFor}` : ""}`}
        tone="pink"
      >
        <div className="flex items-center gap-3 px-4 pt-3.5 pb-2.5 flex-wrap">
          <span className="text-base font-semibold">#{task.id.slice(0, 6)}</span>
          <button
            type="button"
            onClick={onSelect}
            className="text-base text-left hover:text-primary cursor-pointer min-w-0 break-words"
          >
            {sessionLabel(task)}
          </button>
          <span className="text-xs text-dim2">{task.repo}</span>
          
          {todos.length > 0 && (
            <div className="ml-auto flex items-center gap-2">
              <TickBar todos={todos} blocked cell="w-4" />
              <span className="text-xs text-dim2">{todoProgress(todos)}</span>
            </div>
          )}
        </div>
        <DecisionInline task={task} summary={summary} onOpenSession={onSelect} reload={reload} />
      </NotchCard>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// One row of "finished while you were away" — green for a real result, orange
// for a failure. A failure gets the two things a human actually needs next:
// the log that says why, and a retry.
function FinishedRow({
  task,
  onSelect,
  onRetry,
  onOpenLogs,
  onDelete,
  reload,
}: {
  task: Session;
  onSelect: () => void;
  onRetry: () => void;
  onOpenLogs: () => void;
  onDelete: () => void;
  reload: () => void;
}) {
  const failed = task.podPhase === "POD_PHASE_CRASHED";
  const pr = null as { label: string; className: string } | null;
  return (
    <div className="relative">
      <div
        className={`flex items-center gap-3 px-4 py-2.5 border pr-8 flex-wrap ${
          failed ? "border-orange-line bg-orange-bg" : "border-green-line bg-green-bg"
        }`}
      >
        <span className={`w-[7px] h-[7px] rounded-full flex-none ${failed ? "bg-warning" : "bg-success"}`} />
        <span className="text-base font-semibold">#{task.id.slice(0, 6)}</span>
        <button type="button" onClick={onSelect} className="text-base text-left hover:text-primary cursor-pointer min-w-0 break-words">
          {sessionLabel(task)}
        </button>
        <span className="text-xs text-dim2">{task.repo}</span>
        {failed ? (
          <span className="ml-auto text-sm text-warning min-w-0 truncate" title={task.lastError}>
            {"crashed"}
            {task.lastError ? ` · ${task.lastError}` : ""}
          </span>
        ) : (
          <span className="ml-auto text-sm text-dim">{pr ? pr.label : "no PR"}</span>
        )}
        {failed ? (
          <>
            <button type="button" onClick={onOpenLogs} className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary">
              read log
            </button>
            <button type="button" onClick={onRetry} className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary">
              retry
            </button>
          </>
        ) : null}
        {/*
          This row is exactly the "I'm done with this" moment, and until now
          the only thing offered here was delete — which destroys the
          transcript, the sole record of a session that produced no PR.
          Archiving keeps the history and is the fleet's one terminal state.
        */}
        <button
          type="button"
          onClick={() => {
            void client.archiveSession({ sessionId: task.id }).then(reload);
          }}
          className="flex-none border border-acc-line px-3 py-1 text-sm hover:border-primary hover:text-primary"
        >
          archive
        </button>
      </div>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// The WORKING table: nothing here needs a human, so it's dense and calm. The
// live tool line is what makes "is it actually doing something" answerable
// without opening the session.
function WorkingRow({
  task,
  summary,
  last,
  onSelect,
  onDelete,
}: {
  task: Session;
  summary?: ListSummary;
  last: boolean;
  onSelect: () => void;
  onDelete: () => void;
}) {
  const todos = summary?.todos ?? [];
  const inFlight = summary?.inFlight ?? null;
  const provisioning = task.podPhase === "POD_PHASE_PROVISIONING";
  const live = isPodPhaseLive(task.podPhase) && !provisioning;
  const stale = staleBadge(task);

  return (
    <div className={`relative ${last ? "" : "border-b border-line3"}`}>
      <div className="flex items-center gap-3 px-4 py-2.5 pr-8">
        <span
          className={`w-[7px] h-[7px] rounded-full flex-none ${
            stale ? "bg-error" : live ? "bg-info animate-fpulse" : "border border-dim2"
          }`}
        />
        <span className={`text-sm flex-none ${live ? "text-text2" : "text-dim2"}`}>#{task.id.slice(0, 6)}</span>
        <button
          type="button"
          onClick={onSelect}
          className={`text-base text-left hover:text-primary cursor-pointer min-w-0 truncate ${live ? "" : "text-dim"}`}
        >
          {sessionLabel(task)}
        </button>
        <span className="text-xs text-dim2 flex-none">{task.repo}</span>
        {task.permissionMode === "bypassPermissions" && (
          <span
            className="text-xs text-warning border border-orange-line px-1.5 py-px flex-none"
            title="every tool call runs without asking"
          >
            bypass
          </span>
        )}

        {provisioning ? (
          <>
            <span className="ml-auto text-sm text-dim2 min-w-0 truncate">
              booting{task.podMessage ? ` · ${task.podMessage}` : ""}
            </span>
            <span className="w-[105px] h-[3px] bar-provisioning flex-none" />
            <span className="text-xs text-dim2 w-6 text-right flex-none">—</span>
          </>
        ) : (
          <>
            <span className="ml-auto text-sm text-dim min-w-0 truncate">
              {inFlight
                ? `⟳ ${inFlight.tool.toLowerCase()} · ${inFlight.summary}${
                    inFlight.elapsedSeconds !== null ? ` · ${inFlight.elapsedSeconds}s` : ""
                  }`
                : stale
                  ? stale.label.toLowerCase()
                  : "idle"}
            </span>
            <TickBar todos={todos} cell="w-4" className="flex-none" />
            <span className="text-xs text-dim2 w-6 text-right flex-none">
              {todos.length > 0 ? todoProgress(todos) : "—"}
            </span>
          </>
        )}
      </div>
      <DeleteButton onDelete={onDelete} />
    </div>
  );
}

// The collapsed tail. A session in here needs nothing; it exists so the count is
// honest and the row is still reachable, not to be read.
function QuietGroup({
  title,
  tasks,
  onSelect,
}: {
  title: string;
  tasks: Session[];
  onSelect: (id: string) => void;
}) {
  if (tasks.length === 0) return null;
  return (
    <Collapse
      summary={<span className="text-xs text-dim2">▸ {title} · {tasks.length}</span>}
      summaryClassName="py-1"
      contentClassName="pl-3 py-1 flex flex-col gap-1"
    >
      {tasks.map((t) => {
        const badge = sessionBadge(t);
        return (
          <button
            key={t.id}
            type="button"
            onClick={() => onSelect(t.id)}
            className="flex items-center gap-2.5 text-left hover:text-primary cursor-pointer"
          >
            <span className="text-xs text-dim2 flex-none">#{t.id.slice(0, 6)}</span>
            <span className="text-sm text-dim min-w-0 truncate flex-1">{sessionLabel(t)}</span>
            {badge && (
              <span className={`text-2xs px-1 border tracking-wide flex-none ${badge.className}`} title={badge.title}>
                {badge.label}
              </span>
            )}
          </button>
        );
      })}
    </Collapse>
  );
}

export function TaskList({
  tasks,
  summaries,
  needsYouIds,
  onSelect,
  onDelete,
  onRetry,
  onOpenLogs,
  reload,
}: {
  tasks: Session[];
  summaries: Map<string, ListSummary>;
  needsYouIds: Set<string>;
  onSelect: (id: string) => void;
  onDelete: (id: string) => void;
  onRetry: (id: string) => void;
  onOpenLogs: (id: string) => void;
  reload: () => void;
}) {
  // Every bucket bucketTasks returns must be rendered somewhere below.
  //
  // `archived` and `swept` were computed and then dropped, which is the exact
  // failure the function's own comment warns about one level up: a session in
  // no RENDERED group vanishes from the list entirely. Archived sessions —
  // the only ones a human has explicitly finished — were invisible on both
  // form factors. bucketTasks.test.ts pins the coverage of the destructure so
  // the next bucket added cannot be silently dropped the same way.
  const { needsYou, working, finished, stalled, quiet, archived, swept } = bucketTasks(tasks, needsYouIds);

  if (tasks.length === 0) {
    return <div className="p-5 text-base text-dim">No sessions.</div>;
  }

  return (
    <div className="flex-1 min-h-0 overflow-y-auto px-4.5 pt-5 pb-6 flex flex-col gap-3">
      {needsYou.length > 0 && (
        <>
          <SectionHeading title="NEEDS YOU" tone="pink" className="mb-0.5" />
          {needsYou.map((t) => (
            <NeedsYouCard
              key={t.id}
              task={t}
              summary={summaries.get(t.id)}
              onSelect={() => onSelect(t.id)}
              onDelete={() => onDelete(t.id)}
              reload={reload}
            />
          ))}
        </>
      )}

      {finished.length > 0 && (
        <>
          <SectionHeading title="FINISHED WHILE YOU WERE AWAY" tone="green" className="mt-4" />
          {finished.map((t) => (
            <FinishedRow
              key={t.id}
              task={t}
              onSelect={() => onSelect(t.id)}
              onRetry={() => onRetry(t.id)}
              onOpenLogs={() => onOpenLogs(t.id)}
              onDelete={() => onDelete(t.id)}
              reload={reload}
            />
          ))}
        </>
      )}

      {working.length > 0 && (
        <>
          <SectionHeading title="WORKING" note="nothing needed from you" className="mt-4" />
          <div className="border border-line2">
            {working.map((t, i) => (
              <WorkingRow
                key={t.id}
                task={t}
                summary={summaries.get(t.id)}
                last={i === working.length - 1}
                onSelect={() => onSelect(t.id)}
                onDelete={() => onDelete(t.id)}
              />
            ))}
          </div>
        </>
      )}

      {/*
        The collapsed tail. "proposed by audits" used to sit here fed by a
        hardcoded empty array — a group that could never contain anything,
        left over from when a proposal was a task row. Proposals are their own
        table with their own view on the Audits page now (docs/adr/0048).
      */}
      <div className="flex flex-col gap-1 mt-3.5">
        <QuietGroup title="stalled" tasks={stalled} onSelect={onSelect} />
        <QuietGroup title="idle" tasks={quiet} onSelect={onSelect} />
        <QuietGroup title="archived" tasks={archived} onSelect={onSelect} />
        <QuietGroup title="swept · readable, not resumable" tasks={swept} onSelect={onSelect} />
      </div>
    </div>
  );
}
