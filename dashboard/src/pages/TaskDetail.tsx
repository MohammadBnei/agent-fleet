import { useCallback, useEffect, useRef, useState } from "react";
import { client } from "../connectClient";
import type { Task } from "../gen/agentfleet/v1/core_pb";
import { podStateBadge, staleBadge, heartbeatLabel, prBadge, isPodPhaseLive } from "./TaskList";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import {
  parseQuestions,
  parseAnswers,
  findPendingQuestion,
  findPendingPermissions,
  resolvedPermissionDecisions,
  isCogitating,
  latestToolCallSummary,
  latestSlashCommands,
  parseSdkSystemInfo,
  parseSdkResultSummary,
  buildToolCallPairs,
  pairedResultSeqs,
  latestTodos,
  asDisplayMarkdown,
} from "../transcript";
import { useTaskDetail } from "../useTaskDetail";
import { useAtBottom } from "../useAtBottom";
import { useLocalStorageState } from "../useLocalStorageState";
import { ToolCallItem } from "../components/ToolCallItem";
import { ToolCallLine } from "../components/ToolCallLine";
import { PlanCard } from "../components/PlanCard";
import { PermissionCard } from "../components/PermissionCard";
import { ErrorModal } from "../components/ErrorModal";
import { Markdown } from "../components/Markdown";
import { BypassConfirmModal } from "../components/BypassConfirmModal";
import { Panel, type PanelSize } from "../components/Panel";
import { E2eCard } from "../components/E2eCard";
import { Modal } from "../components/Modal";
import { ActionsMenu } from "../components/ActionsMenu";

// Minimal distinct treatment for the raw SDK message types (reliability-
// findings.md #0: "relay everything, let the UI decide") — before this,
// everything but QUESTION/ANSWER fell through to one generic prose bubble.
// ASSISTANT/USER (tool_use/tool_result) aren't handled here — they move to
// the right panel's TOOL CALLS section as paired ToolCallItems instead of
// two unrelated bubbles in the feed. Cosmetic only: no new state, no new RPCs.
function EntryBubble({ entry }: { entry: TranscriptEntry }) {
  if (entry.type === TranscriptEntryType.SYSTEM) {
    const info = parseSdkSystemInfo(entry.text);
    return (
      <div className="text-[10.5px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-ghost badge-xs">session</span>
        {info?.model ? `started · ${info.model}` : "session started"}
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.RESULT) {
    const r = parseSdkResultSummary(entry.text);
    return (
      <div className="text-[10.5px] text-base-content/40">
        {r ? `${r.numTurns ?? "?"} turns · $${(r.totalCostUsd ?? 0).toFixed(3)}` : entry.text}
      </div>
    );
  }
  // Backend-verified events (a real transcript row core only writes after a
  // genuine dashboard/Discord action) get the same compact log-line
  // treatment as SYSTEM/RESULT above, distinct from the generic prose bubble
  // below — otherwise these are indistinguishable from the model's own
  // narration of the same claim (e.g. "good, user approved" in its own
  // text), which is real but unverified.
  if (entry.type === TranscriptEntryType.APPROVE) {
    return (
      <div className="text-[10.5px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-success badge-xs">approved</span>
        plan approved — implementing
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.ABORT) {
    return (
      <div className="text-[10.5px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-warning badge-xs">killed</span>
        {entry.text || "task killed"}
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.INTERRUPT) {
    return (
      <div className="text-[10.5px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-info badge-xs">interrupted</span>
        {entry.text || "current turn interrupted"}
      </div>
    );
  }
  if (entry.type === TranscriptEntryType.PERMISSION_MODE) {
    return (
      <div className="text-[10.5px] text-base-content/40 flex items-center gap-1.5">
        <span className="badge badge-info badge-xs">mode</span>
        permission mode → {entry.text}
      </div>
    );
  }
  return (
    <div className="max-w-[760px]">
      <div className={`text-[12px] leading-relaxed ${entry.from === "human" ? "text-primary" : "text-base-content/90"}`}>
        <Markdown text={asDisplayMarkdown(entry)} />
      </div>
    </div>
  );
}

// Renders one AskUserQuestion batch as a form (already answered: shows what
// was submitted instead). One question per fieldset, chip-style buttons for
// single-select options (mirrors the "herd" mock's quick-reply chips —
// same real options the native tool sends, just restyled), checkboxes for
// multiSelect since a chip can't represent "toggle and combine" cleanly.
function QuestionCard({
  entry,
  answer,
  busy,
  onSubmit,
}: {
  entry: TranscriptEntry;
  answer: Record<string, string> | null;
  busy: boolean;
  onSubmit: (answers: Record<string, string>) => void;
}) {
  const [selected, setSelected] = useState<Record<number, string[]>>({});
  const questions = parseQuestions(entry.text);

  if (!questions) {
    return (
      <div className="px-3 py-2 rounded-md bg-base-200/60 border border-base-content/10">
        <div className="text-[12px] whitespace-pre-wrap">{entry.text}</div>
      </div>
    );
  }

  function toggle(qIndex: number, label: string, multiSelect: boolean | undefined) {
    setSelected((prev) => {
      const current = prev[qIndex] ?? [];
      if (multiSelect) {
        return {
          ...prev,
          [qIndex]: current.includes(label) ? current.filter((l) => l !== label) : [...current, label],
        };
      }
      return { ...prev, [qIndex]: [label] };
    });
  }

  const allAnswered = questions.every((_, i) => (selected[i] ?? []).length > 0);

  function submit() {
    if (!questions) return;
    const answers: Record<string, string> = {};
    questions.forEach((q, i) => {
      answers[q.question] = (selected[i] ?? []).join(", ");
    });
    onSubmit(answers);
  }

  return (
    <div
      className="rounded-lg border p-4 max-w-[760px]"
      style={{
        borderColor: "rgba(224,169,78,.45)",
        background: "linear-gradient(180deg,rgba(224,169,78,.09),rgba(224,169,78,.03))",
      }}
    >
      <div className="flex items-center gap-2">
        {!answer && <span className="w-1.5 h-1.5 rounded-full bg-primary animate-fpulse" />}
        <span className="text-[9.5px] tracking-[0.1em] font-semibold text-primary">
          {answer ? "ANSWERED" : "QUESTION"}
        </span>
      </div>
      <div className="flex flex-col gap-4 mt-3">
        {questions.map((q, qIndex) => (
          <fieldset key={qIndex} className="flex flex-col gap-2">
            <legend className="text-[13px] leading-relaxed text-base-content">
              <span className="badge badge-ghost badge-sm mr-2">{q.header}</span>
              {q.question}
            </legend>
            {answer ? (
              <div className="badge badge-outline">{answer[q.question] ?? "—"}</div>
            ) : q.multiSelect ? (
              q.options.map((opt) => (
                <label key={opt.label} className="flex items-start gap-2 cursor-pointer text-[11px]">
                  <input
                    type="checkbox"
                    className="checkbox checkbox-sm mt-0.5"
                    checked={(selected[qIndex] ?? []).includes(opt.label)}
                    onChange={() => toggle(qIndex, opt.label, true)}
                  />
                  <span>
                    <span className="font-medium">{opt.label}</span>
                    {opt.description && <span className="opacity-60"> — {opt.description}</span>}
                  </span>
                </label>
              ))
            ) : (
              <div className="flex flex-wrap gap-1.5">
                {q.options.map((opt) => {
                  const active = (selected[qIndex] ?? []).includes(opt.label);
                  return (
                    <button
                      key={opt.label}
                      type="button"
                      title={opt.description}
                      onClick={() => toggle(qIndex, opt.label, false)}
                      className={`px-2.5 py-1 rounded-full text-[10.5px] border cursor-pointer ${
                        active
                          ? "border-primary/60 bg-primary/15 text-primary"
                          : "border-base-content/15 text-base-content/70 hover:border-base-content/30"
                      }`}
                    >
                      {opt.label}
                    </button>
                  );
                })}
              </div>
            )}
          </fieldset>
        ))}
        {!answer && (
          <button
            type="button"
            className="btn btn-primary btn-sm self-start"
            disabled={busy || !allAnswered}
            onClick={submit}
          >
            Submit answer
          </button>
        )}
      </div>
    </div>
  );
}

const DEFAULT_HEIGHT = 224; // matches TODOS's prior max-h-56
const PANEL_ORDER = ["todos", "toolcalls", "changes"] as const;

const STATUS_COLOR: Record<string, string> = {
  pending: "text-base-content/60 border-base-content/20 bg-base-content/5",
  claimed: "text-info border-info/45 bg-info/10",
  running: "text-info border-info/45 bg-info/10",
  done: "text-success border-success/45 bg-success/10",
  failed: "text-warning border-warning/45 bg-warning/10",
  cancelled: "text-warning border-warning/45 bg-warning/10",
};

export function TaskDetail({
  taskId,
  tasks,
  onSelect,
}: {
  taskId: string;
  tasks: Task[];
  onSelect: (id: string) => void;
}) {
  const {
    task: fetchedTask,
    entries,
    previewUrl,
    e2e,
    branch,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    run,
    sendDiscuss,
    clearActionError,
  } = useTaskDetail(taskId);
  const [message, setMessage] = useState("");
  const [bypassOpen, setBypassOpen] = useState(false);
  const { ref: feedRef, atBottom: feedAtBottom, onScroll: feedOnScroll, scrollToBottom: feedScrollToBottom } =
    useAtBottom<HTMLDivElement>();
  const { ref: todosRef, atBottom: todosAtBottom, onScroll: todosOnScroll, scrollToBottom: todosScrollToBottom } =
    useAtBottom<HTMLDivElement>();

  // A new human entry (this tab's own send, or one relayed from Discord)
  // always jumps to the bottom — the reader clearly wants to see it. A new
  // agent entry follows too, but only if the reader was already at the
  // bottom (feedAtBottom, captured as of just before this entry landed) —
  // otherwise they're mid-scroll reading history and it'd yank the view;
  // the button just pulses instead (cleared once they scroll back down
  // themselves, via the effect below).
  const [hasNewAiMessage, setHasNewAiMessage] = useState(false);
  const prevEntriesLenRef = useRef<number | null>(null);
  useEffect(() => {
    if (prevEntriesLenRef.current === null) {
      prevEntriesLenRef.current = entries.length;
      return;
    }
    if (entries.length > prevEntriesLenRef.current) {
      const latest = entries[entries.length - 1];
      if (latest.from === "human" || feedAtBottom) {
        feedScrollToBottom();
        setHasNewAiMessage(false);
      } else {
        setHasNewAiMessage(true);
      }
    }
    prevEntriesLenRef.current = entries.length;
  }, [entries, feedAtBottom, feedScrollToBottom]);

  // Land on the latest message when a task is opened (selecting it from the
  // list, or a direct ?task= nav) — this component instance is reused
  // across task switches (App.tsx doesn't key it by id), so track which
  // task we've already jumped for instead of scrolling on every render.
  // Instant, not smooth — this is "arriving", not "a new message came in"
  // (that's the effect above).
  const scrolledForTaskRef = useRef<string | null>(null);
  useEffect(() => {
    if (entries.length === 0 || scrolledForTaskRef.current === taskId) return;
    scrolledForTaskRef.current = taskId;
    const el = feedRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [taskId, entries, feedRef]);
  useEffect(() => {
    if (feedAtBottom) setHasNewAiMessage(false);
  }, [feedAtBottom]);

  const [sizes, setSizes] = useLocalStorageState<Record<string, PanelSize>>("taskDetail.rightPanel.sizes", {});
  const [collapsed, setCollapsed] = useLocalStorageState<Record<string, boolean>>("taskDetail.rightPanel.collapsed", {});
  const [actionsOpen, setActionsOpen] = useState(false);
  // "Follow" mode for the fit-height button — see toggleAutoFit/the effect
  // below fitPanels.
  const [autoFit, setAutoFit] = useState(false);
  // Plain show/hide, not a text filter — desktop already keeps these out of
  // its main feed (they live in the TOOL CALLS/CHANGES panels instead), so
  // these only ever affect mobile, which mixes them inline into its feed.
  // Shared localStorage keys: settable from here or from mobile's Actions
  // modal, applied by mobile's feed either way.
  const [hideToolsInFeed, setHideToolsInFeed] = useLocalStorageState<boolean>("taskDetail.hideToolsInFeed", false);
  const [hideChangesInFeed, setHideChangesInFeed] = useLocalStorageState<boolean>("taskDetail.hideChangesInFeed", false);
  // Measurement refs for the "fit height" button below — real rendered
  // DOM, not assumed pixel constants.
  const sidebarRef = useRef<HTMLDivElement>(null);
  // Wraps the fit button *and* the e2e card — anything fixed-height that
  // isn't in PANEL_ORDER has to live in here, so fitPanels counts it as
  // overhead (and as one flex child, one gap) instead of handing the panels
  // more height than the sidebar actually has.
  const fitButtonRef = useRef<HTMLDivElement>(null);
  const panelRootRefs = useRef<Record<string, HTMLDivElement | null>>({});
  // Sidebar/exchange-zone split width — persisted only on drag-end (one
  // write), driven live during the drag by a plain useState so dragging
  // doesn't hammer localStorage on every mousemove.
  const [sidebarWidth, setSidebarWidth] = useLocalStorageState<number>("taskDetail.sidebarWidth", 280);
  const [liveSidebarWidth, setLiveSidebarWidth] = useState<number | null>(null);
  const displaySidebarWidth = liveSidebarWidth ?? sidebarWidth;

  function startSidebarResize(e: React.MouseEvent) {
    e.preventDefault();
    const startX = e.clientX;
    const startWidth = sidebarWidth;
    function onMove(ev: MouseEvent) {
      setLiveSidebarWidth(Math.min(600, Math.max(200, startWidth - (ev.clientX - startX))));
    }
    function onUp() {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
      setLiveSidebarWidth((v) => {
        if (v !== null) setSidebarWidth(v);
        return null;
      });
    }
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  function handleResize(id: string, size: PanelSize) {
    // A manual drag conflicts with follow mode — left on, the next window
    // resize would immediately overwrite what the user just set by hand.
    setAutoFit(false);
    setSizes((prev) => ({ ...prev, [id]: size }));
  }

  function toggleCollapsed(id: string) {
    setCollapsed((prev) => ({ ...prev, [id]: !prev[id] }));
  }

  // Distributes the sidebar's real available height across whichever
  // panels are currently expanded — one taking it all, several splitting
  // it evenly — instead of the user dragging each resize handle by hand.
  // Measures actual rendered chrome per panel (header + padding) rather
  // than assuming a fixed overhead, so it stays correct if a header ever
  // grows (e.g. the tool filter input making TOOL CALLS' header taller).
  // Memoized (real deps only — setSizes is itself stable now) so the
  // follow-mode effect below can depend on it without rebinding its resize
  // listener on every unrelated render.
  const fitPanels = useCallback(() => {
    const sidebarEl = sidebarRef.current;
    if (!sidebarEl) return;
    const cs = getComputedStyle(sidebarEl);
    const available = sidebarEl.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom);
    const gapPx = parseFloat(cs.rowGap || "0") || 0;

    const expandedIds = PANEL_ORDER.filter((id) => !collapsed[id]);
    if (expandedIds.length === 0) return;

    // Sidebar's flex children are [fit button, ...3 panels] — 3 gaps, not 2.
    let overhead = gapPx * PANEL_ORDER.length + (fitButtonRef.current?.offsetHeight ?? 0);
    for (const id of PANEL_ORDER) {
      const el = panelRootRefs.current[id];
      if (!el) continue;
      overhead += collapsed[id] ? el.offsetHeight : el.offsetHeight - (sizes[id]?.height ?? DEFAULT_HEIGHT);
    }

    const perPanel = Math.max(72, Math.floor((available - overhead) / expandedIds.length));
    setSizes((prev) => {
      const next = { ...prev };
      for (const id of expandedIds) next[id] = { height: perPanel };
      return next;
    });
  }, [collapsed, sizes, setSizes]);

  function toggleAutoFit() {
    const next = !autoFit;
    setAutoFit(next);
    if (next) fitPanels();
  }

  // "Follow" mode: once turned on, a window resize re-runs fitPanels
  // instead of the fit only ever applying once at click time. Re-subscribes
  // whenever the state fitPanels reads changes, so a resize mid-session
  // doesn't call a stale closure.
  useEffect(() => {
    if (!autoFit) return;
    function onWindowResize() {
      fitPanels();
    }
    window.addEventListener("resize", onWindowResize);
    return () => window.removeEventListener("resize", onWindowResize);
  }, [autoFit, fitPanels]);

  if (loadError) return <div className="alert alert-error m-4">{loadError}</div>;
  if (!fetchedTask) return <div className="p-4">Loading…</div>;

  // `tasks` is App.tsx's already-polled (5s) list — preferring it here
  // keeps pod_phase/status live without a second poll loop just for this
  // page; falls back to the one-shot fetch for the instant before the next
  // poll tick includes this task (e.g. a just-created task).
  const task = tasks.find((t) => t.id === taskId) ?? fetchedTask;

  const siblings = tasks.filter((t) => t.id !== taskId);
  const podBadge = podStateBadge(task);
  const staleTag = staleBadge(task);
  const heartbeat = heartbeatLabel(task);
  const prLink = prBadge(task);

  function sendMessage() {
    const text = message.trim();
    if (!text) return;
    setMessage("");
    sendDiscuss(text);
  }

  // Used both by the inline QuestionCard below and the quick-reply chip row.
  const pendingQuestion = findPendingQuestion(entries);
  const pendingParsed = pendingQuestion ? parseQuestions(pendingQuestion.text) : null;
  const chipQuestion =
    pendingParsed && pendingParsed.length === 1 && !pendingParsed[0].multiSelect ? pendingParsed[0] : null;
  const changes = latestToolCallSummary(entries)?.files ?? null;
  const pendingPermissions = findPendingPermissions(entries);
  const permissionDecisions = resolvedPermissionDecisions(entries);
  const cogitating = isCogitating(entries, isPodPhaseLive(task.podPhase), pendingPermissions.length > 0, pendingQuestion !== null);
  const toolCallPairs = buildToolCallPairs(entries);
  const toolCallPairsBySeq = new Map(toolCallPairs.map((p) => [p.call.seq, p]));
  const consumedResultSeqs = pairedResultSeqs(toolCallPairs);
  const todos = latestTodos(entries);
  // Lean name-only autocomplete (the SDK's system/init message carries no
  // descriptions/argument hints at runtime, docs/adr/0027) — only shown
  // while the draft looks like a command, filtered by what's typed so far.
  const slashCommands = message.startsWith("/")
    ? (latestSlashCommands(entries) ?? []).filter((c) => c.toLowerCase().startsWith(message.slice(1).toLowerCase()))
    : [];

  return (
    <div className="grid grid-rows-[auto_1fr] h-full">
      <div className="px-6 pt-4 pb-3.5 border-b border-base-content/10">
        <div className="flex items-center gap-2.5">
          <h2 className="font-display font-semibold text-[19px]">{task.description}</h2>
          <span className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide ${STATUS_COLOR[task.status] ?? "border-base-content/20"}`}>
            {task.status.toUpperCase()}
          </span>
          {podBadge && (
            <span
              className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide ${podBadge.className}`}
              title={task.podMessage || undefined}
            >
              POD {podBadge.label}
            </span>
          )}
          {cogitating && (
            <span className="px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide border-info/45 bg-info/10 text-info flex items-center gap-1">
              <span className="w-1.5 h-1.5 rounded-full bg-info animate-fpulse" />
              THINKING
            </span>
          )}
          {staleTag && (
            <span
              className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide ${staleTag.className}`}
              title={staleTag.title}
            >
              {staleTag.label}
            </span>
          )}
          {prLink && (
            <a
              href={task.prUrl}
              target="_blank"
              rel="noreferrer"
              className={`px-2 py-0.5 rounded text-[9.5px] font-semibold border tracking-wide border-current ${prLink.className}`}
            >
              {prLink.label}
            </a>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-3.5 mt-2 text-[10px] text-base-content/50">
          <span>#{task.id.slice(0, 6)}</span>
          <span>{task.repo}</span>
          {branch && <span>branch {branch}</span>}
          {heartbeat && <span className={staleTag ? "text-error" : undefined}>{heartbeat}</span>}
          {task.retryCount > 0 && <span>attempt {task.retryCount + 1}</span>}
          {task.lastError && (
            <span className="text-warning" title={task.lastError}>
              last error ⓘ
            </span>
          )}
        </div>
      </div>

      <div className="grid min-h-0" style={{ gridTemplateColumns: `1fr 5px ${displaySidebarWidth}px` }}>
        <div className="min-w-0 min-h-0 flex flex-col">
          <div className="relative flex-1 min-h-0">
          <div ref={feedRef} onScroll={feedOnScroll} className="absolute inset-0 overflow-y-auto px-6 py-5 flex flex-col gap-3">
            {entries.map((entry, idx) => {
              // Rendered inline by its QUESTION entry below, not as its own bubble.
              if (entry.type === TranscriptEntryType.ANSWER) return null;
              // The sidecar's periodic {branch, files[]} push — always
              // summarized in the CHANGES panel regardless; inline here too
              // when the shared "Changes" toggle (Actions modal) is on.
              if (entry.type === TranscriptEntryType.TOOL_CALL) {
                if (hideChangesInFeed) return null;
                return <ToolCallLine key={String(entry.seq)} entry={entry} />;
              }
              // Rendered inline by its PERMISSION_REQUEST entry below, not
              // as its own bubble.
              if (entry.type === TranscriptEntryType.PERMISSION_RESPONSE) return null;
              // Every canUseTool prompt always gets its own full-width
              // card, ignoring the "Tools" toggle — it's a decision that
              // needs a response, not an incidental tool call. ExitPlanMode
              // gets the nicer plan-specific PlanCard; everything else gets
              // the generic PermissionCard. Must be checked before the
              // generic ASSISTANT branch below (the raw tool_use log entry
              // for the same call still falls through there separately).
              if (entry.type === TranscriptEntryType.PERMISSION_REQUEST) {
                let parsed: { tool?: string; input?: unknown } = {};
                try {
                  parsed = JSON.parse(entry.text);
                } catch {
                  return null;
                }
                if (typeof parsed.tool !== "string") return null;
                const isPending = pendingPermissions.some((p) => p.entry.seq === entry.seq);
                const permissionKey = `permission:${entry.seq}`;
                const respond = (decision: { behavior: "allow" | "deny"; message?: string }) =>
                  run(() => client.respondToPermission({ taskId, seq: entry.seq, decisionJson: JSON.stringify(decision) }), permissionKey);
                if (parsed.tool === "ExitPlanMode") {
                  const plan = (parsed.input as { plan?: string } | undefined)?.plan;
                  if (typeof plan !== "string") return null;
                  return (
                    <PlanCard
                      key={String(entry.seq)}
                      plan={plan}
                      pending={isPending}
                      busy={busyKey === permissionKey}
                      decision={permissionDecisions.get(entry.seq)}
                      onApprove={() => respond({ behavior: "allow" })}
                      onFeedback={(text) => sendDiscuss(text)}
                      edgeClassName="-mx-6 px-6"
                    />
                  );
                }
                return (
                  <PermissionCard
                    key={String(entry.seq)}
                    tool={parsed.tool}
                    input={parsed.input}
                    pending={isPending}
                    busy={busyKey === permissionKey}
                    decision={permissionDecisions.get(entry.seq)}
                    onAllow={() => respond({ behavior: "allow" })}
                    onDeny={(message) => respond({ behavior: "deny", message })}
                    edgeClassName="-mx-6 px-6"
                  />
                );
              }
              // Always listed in the TOOL CALLS panel regardless; inline
              // here too when the shared "Tools" toggle (Actions modal) is
              // on — same paired call+result treatment as mobile's feed.
              if (entry.type === TranscriptEntryType.ASSISTANT) {
                if (hideToolsInFeed) return null;
                const pair = toolCallPairsBySeq.get(entry.seq);
                return pair ? <ToolCallItem key={String(entry.seq)} pair={pair} /> : null;
              }
              // Already rendered inside its pair's ToolCallItem above when
              // shown; an orphaned tool_result (no matching call) still
              // falls through to EntryBubble's default bubble.
              if (entry.type === TranscriptEntryType.USER && consumedResultSeqs.has(entry.seq)) return null;

              if (entry.type === TranscriptEntryType.QUESTION) {
                const answerEntry = entries.slice(idx + 1).find((e) => e.type === TranscriptEntryType.ANSWER);
                const questionKey = `question:${entry.seq}`;
                return (
                  <QuestionCard
                    key={String(entry.seq)}
                    entry={entry}
                    answer={answerEntry ? parseAnswers(answerEntry.text) : null}
                    busy={busyKey === questionKey}
                    onSubmit={(answers) =>
                      run(
                        () => client.answerQuestion({ taskId, seq: entry.seq, answersJson: JSON.stringify({ answers }) }),
                        questionKey,
                      )
                    }
                  />
                );
              }

              return <div key={String(entry.seq)}><EntryBubble entry={entry} /></div>;
            })}
            {pendingMessage && (
              <div className="max-w-[760px] opacity-60">
                <div className="text-[12px] leading-relaxed text-primary flex items-start gap-2">
                  <div className="flex-1 min-w-0">
                    <Markdown text={asDisplayMarkdown({ from: "human", text: pendingMessage })} />
                  </div>
                  <span className="loading loading-spinner loading-xs flex-none mt-1" />
                </div>
              </div>
            )}
          </div>
          {!feedAtBottom && (
            <button
              type="button"
              onClick={feedScrollToBottom}
              className="btn btn-circle btn-xs absolute bottom-3 left-1/2 -translate-x-1/2 bg-base-300 border-base-content/20 shadow-md"
              title="Scroll to bottom"
            >
              ↓
              {hasNewAiMessage && (
                <span className="absolute -top-0.5 -right-0.5 w-1.5 h-1.5 rounded-full bg-primary animate-fpulse" />
              )}
            </button>
          )}
          </div>

          <div className="flex-none px-6 py-3.5 border-t border-base-content/10 bg-base-200 flex flex-col gap-2.5">
            <ErrorModal message={actionError} onClose={clearActionError} />
            {chipQuestion && (
              <div className="flex gap-1.5 items-center flex-wrap">
                <span className="text-[9.5px] tracking-[0.09em] text-base-content/40 mr-1">QUICK</span>
                {chipQuestion.options.map((opt) => (
                  <button
                    key={opt.label}
                    type="button"
                    disabled={busyKey !== null}
                    title={opt.description}
                    onClick={() =>
                      pendingQuestion &&
                      run(
                        () =>
                          client.answerQuestion({
                            taskId,
                            seq: pendingQuestion.seq,
                            answersJson: JSON.stringify({ answers: { [chipQuestion.question]: opt.label } }),
                          }),
                        `question:${pendingQuestion.seq}`,
                      )
                    }
                    className="px-2.5 py-1 rounded-full text-[10.5px] border border-base-content/15 text-base-content/70 hover:border-primary/50 hover:text-primary cursor-pointer disabled:opacity-40"
                  >
                    {opt.label}
                  </button>
                ))}
              </div>
            )}
            {slashCommands.length > 0 && (
              <div className="flex gap-1.5 items-center flex-wrap">
                <span className="text-[9.5px] tracking-[0.09em] text-base-content/40 mr-1">COMMANDS</span>
                {slashCommands.map((c) => (
                  <button
                    key={c}
                    type="button"
                    onClick={() => setMessage(`/${c} `)}
                    className="px-2.5 py-1 rounded-full text-[10.5px] border border-base-content/15 text-base-content/70 hover:border-primary/50 hover:text-primary cursor-pointer"
                  >
                    /{c}
                  </button>
                ))}
              </div>
            )}
            <BypassConfirmModal
              open={bypassOpen}
              onCancel={() => setBypassOpen(false)}
              onConfirm={() => {
                setBypassOpen(false);
                run(() => client.setPermissionMode({ taskId, mode: "bypassPermissions" }), "bypass");
              }}
            />
            <div className="flex items-stretch gap-2">
              <div className="flex-1 flex items-center gap-2.5 px-3.5 py-3 border border-base-content/15 rounded-lg bg-base-300/40">
                <span className="text-primary font-semibold">&gt;</span>
                <input
                  value={message}
                  onChange={(e) => setMessage(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") sendMessage();
                  }}
                  disabled={busyKey !== null}
                  placeholder="message the agent…"
                  className="flex-1 bg-transparent outline-none text-[12px] placeholder:text-base-content/40"
                />
                <button
                  type="button"
                  disabled={busyKey !== null || !message.trim()}
                  onClick={sendMessage}
                  className="text-[11px] text-primary font-medium disabled:opacity-30 disabled:cursor-not-allowed"
                >
                  Send
                </button>
              </div>
              <button
                type="button"
                onClick={() => setActionsOpen(true)}
                title="Actions"
                className="flex-none flex items-center justify-center px-3 border border-base-content/15 rounded-lg text-base-content/60 hover:text-base-content hover:border-base-content/30 cursor-pointer"
              >
                ⚙
              </button>
              <Modal open={actionsOpen} onClose={() => setActionsOpen(false)}>
                <h3 className="font-semibold text-base mb-3">Actions</h3>
                <ActionsMenu
                  taskId={taskId}
                  busy={busyKey !== null}
                  run={run}
                  previewUrl={previewUrl}
                  currentMode={task.permissionMode}
                  podPhase={task.podPhase}
                  onBypassClick={() => {
                    setActionsOpen(false);
                    setBypassOpen(true);
                  }}
                  hideToolsInFeed={hideToolsInFeed}
                  onHideToolsInFeedChange={setHideToolsInFeed}
                  hideChangesInFeed={hideChangesInFeed}
                  onHideChangesInFeedChange={setHideChangesInFeed}
                />
              </Modal>
            </div>
          </div>
        </div>

        <div
          onMouseDown={startSidebarResize}
          title="Drag to resize"
          className="cursor-col-resize bg-base-content/5 hover:bg-primary/40 active:bg-primary/50"
        />

        <div ref={sidebarRef} className="border-l border-base-content/10 bg-base-200 px-1.5 py-1.5 overflow-y-auto min-h-0 flex flex-col gap-1.5">
          <div ref={fitButtonRef} className="flex flex-col gap-1.5">
            <div className="flex items-center justify-end">
              <button
                type="button"
                onClick={toggleAutoFit}
                title={
                  autoFit
                    ? "Following window resizes — click to stop"
                    : "Fill available height with the expanded panel(s) — split evenly if more than one, and keep following window resizes"
                }
                className={`flex-none text-[9px] px-1.5 py-0.5 rounded border cursor-pointer ${
                  autoFit
                    ? "text-primary border-primary/40 bg-primary/10"
                    : "text-base-content/50 hover:text-base-content/80 border-base-content/10 hover:border-base-content/25"
                }`}
              >
                ⇕ {autoFit ? "following" : "fit height"}
              </button>
            </div>
            <E2eCard e2e={e2e} />
          </div>
          <Panel
            id="todos"
            title="TODOS"
            rootRef={(el) => {
              panelRootRefs.current.todos = el;
            }}
            size={sizes.todos ?? { height: DEFAULT_HEIGHT }}
            onResize={handleResize}
            collapsed={collapsed.todos ?? false}
            onToggleCollapse={() => toggleCollapsed("todos")}
            bodyRef={todosRef}
            onBodyScroll={todosOnScroll}
            overlay={
              !todosAtBottom && (
                <button
                  type="button"
                  onClick={todosScrollToBottom}
                  className="btn btn-circle btn-xs absolute bottom-3 right-3 bg-base-100 border-base-content/20 shadow-md"
                  title="Scroll to bottom"
                >
                  ↓
                </button>
              )
            }
          >
            {todos && todos.length > 0 ? (
              todos.map((t, i) => (
                <div key={i} className="flex gap-2 items-start text-[11px]">
                  <span
                    className={
                      t.status === "completed"
                        ? "text-success"
                        : t.status === "in_progress"
                          ? "text-primary"
                          : "text-base-content/30"
                    }
                  >
                    {t.status === "completed" ? "✓" : t.status === "in_progress" ? "◐" : "○"}
                  </span>
                  <span
                    className={
                      t.status === "completed"
                        ? "line-through text-base-content/40"
                        : t.status === "in_progress"
                          ? "text-base-content"
                          : "text-base-content/80"
                    }
                  >
                    {t.status === "in_progress" ? t.activeForm : t.content}
                  </span>
                </div>
              ))
            ) : (
              <div className="text-[10.5px] text-base-content/40">no todos yet</div>
            )}
          </Panel>

          <Panel
            id="toolcalls"
            title="TOOL CALLS"
            rootRef={(el) => {
              panelRootRefs.current.toolcalls = el;
            }}
            size={sizes.toolcalls ?? { height: DEFAULT_HEIGHT }}
            onResize={handleResize}
            collapsed={collapsed.toolcalls ?? false}
            onToggleCollapse={() => toggleCollapsed("toolcalls")}
          >
            {toolCallPairs.length > 0 ? (
              toolCallPairs.map((pair) => <ToolCallItem key={String(pair.call.seq)} pair={pair} />)
            ) : (
              <div className="text-[10.5px] text-base-content/40">no tool calls yet</div>
            )}
          </Panel>

          <Panel
            id="changes"
            title="CHANGES"
            rootRef={(el) => {
              panelRootRefs.current.changes = el;
            }}
            headerExtra={branch && <span className="text-[9.5px] text-base-content/35">{branch}</span>}
            size={sizes.changes ?? { height: DEFAULT_HEIGHT }}
            onResize={handleResize}
            collapsed={collapsed.changes ?? false}
            onToggleCollapse={() => toggleCollapsed("changes")}
          >
            {changes && changes.length > 0 ? (
              changes.map((c, i) => (
                <div key={i} className="flex items-center gap-2 text-[10.5px] min-w-0">
                  <span className="flex-1 text-base-content/70 truncate min-w-0">{c.path}</span>
                  <span className="text-success flex-none">+{c.added}</span>
                  {c.removed > 0 && <span className="text-warning flex-none">−{c.removed}</span>}
                </div>
              ))
            ) : (
              <div className="text-[10.5px] text-base-content/40">no changes yet</div>
            )}
          </Panel>
        </div>
      </div>

      {siblings.length > 0 && (
        <div className="flex-none border-t border-base-content/10 bg-base-200 px-6 py-2.5 flex items-center gap-2.5 overflow-x-auto">
          <span className="text-[9.5px] tracking-[0.1em] text-base-content/40 flex-none">REST OF THE HERD</span>
          {siblings.map((t) => {
            const siblingPodBadge = podStateBadge(t);
            return (
              <button
                key={t.id}
                type="button"
                onClick={() => onSelect(t.id)}
                className="flex-none flex items-center gap-2 px-2.5 py-1.5 border border-base-content/10 rounded-md hover:border-base-content/25 cursor-pointer"
              >
                <span className="text-[10.5px]">#{t.id.slice(0, 6)}</span>
                <span className={`px-1.5 py-0.5 rounded text-[9px] font-semibold border tracking-wide ${STATUS_COLOR[t.status] ?? "border-base-content/20"}`}>
                  {t.status.toUpperCase()}
                </span>
                {siblingPodBadge && (
                  <span
                    className={`px-1.5 py-0.5 rounded text-[9px] font-semibold border tracking-wide ${siblingPodBadge.className}`}
                    title={t.podMessage || undefined}
                  >
                    {siblingPodBadge.label}
                  </span>
                )}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
