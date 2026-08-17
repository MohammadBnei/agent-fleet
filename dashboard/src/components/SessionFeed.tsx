import { useState } from "react";
import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import {
  asDisplayMarkdown,
  buildToolCallPairs,
  entryTime,
  findPendingPermissions,
  pairedResultSeqs,
  parseAnswers,
  parseSdkSignal,
  parseSdkSystemInfo,
  parseSdkToolResult,
  parseSdkToolUse,
  parseSdkResultSummary,
  permissionDenyMessages,
  resolvedPermissionDecisions,
  resultDelta,
  summarizeToolInput,
  type SdkResultSummary,
  type Density,
  type FeedVisibility,
  type ToolCallPair,
} from "../transcript";
import { Markdown } from "./Markdown";
import { Collapse } from "./Collapse";
import { TranscriptEntryView } from "./TranscriptEntryView";
import { ToolCallDetail } from "./ToolCallItem";
import { ToolCallLine } from "./ToolCallLine";
import { PermissionCard } from "./PermissionCard";
import { PlanCard } from "./PlanCard";
import { QuestionCard } from "./QuestionCard";

// The feed, shared by both detail views.
//
// docs/dashboard-spec.md §3 ranks twelve entry kinds into five tiers and §8
// item 2 records the failure this fixes: a `$0.42 · 7 turns` result line, a
// compaction marker and the agent's actual prose all rendered as ~10px grey
// text, so tier 1 and tier 4 looked identical. Here each tier gets genuinely
// different weight:
//
//   1 demands action  full-width cards (or, on mobile, the docked decision)
//   2 narrative       readable prose, generous leading, a dot or ❯ marker
//   3 tool activity   dense rows grouped into one bordered block per run
//   4 lifecycle       a hairline rule with a label left and a summary right
//   5 alarms          an orange bordered bar — never a log line
//
// Tiers 1 and 5 ignore the density control entirely: they're the reason the
// screen exists.

const CROSS_SESSION = /^\[from session ([^\]]+)\]\s*/;

function QuietRule({ label, detail }: { label: string; detail?: string | null }) {
  return (
    <div className="flex items-center gap-2.5">
      <span className="text-2xs sm:text-xs text-dim2 whitespace-nowrap">{label}</span>
      <span className="flex-1 h-px bg-line3" />
      {detail && <span className="text-2xs sm:text-xs text-dim2 text-right">{detail}</span>}
    </div>
  );
}

function AlarmBar({ text, detail, compact }: { text: React.ReactNode; detail?: string | null; compact?: boolean }) {
  return (
    <div className="border border-orange-line bg-orange-bg px-3 py-2.5 flex gap-2.5 items-start">
      <span className="text-warning text-sm flex-none">!</span>
      <div className={`text-warning ${compact ? "text-xs" : "text-sm"} leading-[1.55] min-w-0`}>
        {text}
        {detail && <div className="text-dim mt-0.5 break-words">{detail}</div>}
      </div>
    </div>
  );
}

// One run of consecutive tool calls. The mockups collapse them into a single
// bordered block with a count and a collapse-all, which is what makes a
// tool-heavy stretch scannable instead of a wall.
function ToolGroup({ pairs, compact }: { pairs: ToolCallPair[]; compact?: boolean }) {
  const [collapsed, setCollapsed] = useState(false);
  const [expanded, setExpanded] = useState<bigint | null>(null);

  return (
    <div>
      <div className="flex items-center gap-2.5 mb-1.5">
        <span className="text-2xs tracking-[0.1em] text-dim2 whitespace-nowrap">
          {pairs.length} TOOL CALL{pairs.length === 1 ? "" : "S"}
        </span>
        <span className="flex-1 h-px bg-line3 sm:order-none order-last" />
        <button
          type="button"
          onClick={() => setCollapsed((v) => !v)}
          className="text-2xs sm:text-xs text-dim hover:text-primary cursor-pointer whitespace-nowrap"
        >
          {collapsed ? "expand" : "collapse"}
        </button>
      </div>
      {!collapsed && (
        <div className="border border-line3">
          {pairs.map((pair, i) => {
            const info = pair.callInfo;
            const failed = pair.resultInfo?.isError === true;
            const inFlight = !pair.result;
            const open = expanded === pair.call.seq;
            return (
              <div key={String(pair.call.seq)} className={i === pairs.length - 1 ? "" : "border-b border-line3"}>
                <button
                  type="button"
                  onClick={() => setExpanded(open ? null : pair.call.seq)}
                  className={`w-full flex gap-2.5 items-center px-2.5 py-1.5 text-left cursor-pointer hover:bg-base-300/40 ${
                    failed ? "bg-orange-wash" : ""
                  }`}
                >
                  <span
                    className={`w-1.5 h-1.5 rounded-full flex-none ${
                      failed ? "bg-warning" : inFlight ? "bg-info animate-fpulse" : "bg-green-dot"
                    }`}
                  />
                  <span className={`text-text2 flex-none ${compact ? "text-xs w-11" : "text-sm w-[70px]"}`}>
                    {info.tool}
                  </span>
                  <span className={`text-dim flex-1 min-w-0 truncate ${compact ? "text-xs" : "text-sm"}`}>
                    {summarizeToolInput(info.input)}
                  </span>
                  <span
                    className={`flex-none text-xs ${
                      failed ? "text-warning" : inFlight ? "text-info" : "text-dim2"
                    }`}
                  >
                    {failed ? "failed" : inFlight ? "running" : "ok"}
                  </span>
                </button>
                {open && (
                  <div className="px-2.5 py-2 border-t border-line3 bg-code">
                    <ToolCallDetail pair={pair} />
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

// The head of the feed when older history exists. One click = one page,
// deliberately: a session's transcript is unbounded, and an infinite scroll
// that keeps fetching as you drag is how you end up holding all of it in
// memory again — the thing loading the newest page first was meant to stop.
export function LoadOlder({ loading, onLoad }: { loading: boolean; onLoad: () => void }) {
  return (
    <div className="flex items-center gap-2.5 mb-1.5">
      <span className="text-2xs tracking-[0.1em] text-dim2 whitespace-nowrap">EARLIER</span>
      <span className="flex-1 h-px bg-line3" />
      <button
        type="button"
        onClick={onLoad}
        disabled={loading}
        className="text-2xs sm:text-xs text-dim hover:text-primary cursor-pointer whitespace-nowrap disabled:cursor-wait"
      >
        {loading ? "loading…" : "load earlier"}
      </button>
    </div>
  );
}

export function SessionFeed({
  entries,
  visibility,
  density,
  busyKey,
  hasOlder = false,
  loadingOlder = false,
  onLoadOlder,
  compact = false,
  // Mobile docks the pending decision above the composer instead of leaving it
  // inline, so the feed must not also render it.
  dockPendingDecision = false,
  onRespond,
  onApprovePlan,
  onAnswer,
  onPlanFeedback,
}: {
  entries: TranscriptEntry[];
  visibility: FeedVisibility;
  density: Density;
  busyKey: string | null;
  hasOlder?: boolean;
  loadingOlder?: boolean;
  onLoadOlder?: () => void;
  compact?: boolean;
  dockPendingDecision?: boolean;
  onRespond: (seq: bigint, decision: { behavior: "allow" | "deny"; message?: string }) => void;
  // Plan approval carries an optional permission-mode switch, so it is its own
  // path rather than an allow decision (approvePlan.ts).
  onApprovePlan: (seq: bigint, mode?: "auto") => void;
  onAnswer: (seq: bigint, answers: Record<string, string>) => void;
  onPlanFeedback: (text: string) => void;
}) {
  const pairs = buildToolCallPairs(entries);
  const pairsBySeq = new Map(pairs.map((p) => [p.call.seq, p]));
  const consumedResults = pairedResultSeqs(pairs);
  const pendingSeqs = new Set(findPendingPermissions(entries).map((p) => p.entry.seq));
  const decisions = resolvedPermissionDecisions(entries);
  const denyMessages = permissionDenyMessages(entries);
  const edge = compact ? "-mx-3.5 px-3.5" : "-mx-4.5 px-4.5";

  // Build TodoWrite toolUseId set upfront to avoid O(n²) scan
  const todoWriteToolUseIds = new Set<string>();
  for (const e of entries) {
    if (e.type === TranscriptEntryType.ASSISTANT) {
      const info = parseSdkToolUse(e.text);
      if (info?.tool === "TodoWrite" && info.id) {
        todoWriteToolUseIds.add(info.id);
      }
    }
  }

  const out: React.ReactNode[] = [];
  if (hasOlder && onLoadOlder) {
    out.push(<LoadOlder key="load-older" loading={loadingOlder} onLoad={onLoadOlder} />);
  }
  // Consecutive tool calls accumulate here and flush as one ToolGroup the
  // moment anything else appears, so the grouping follows the real
  // chronology rather than hoisting every call to one block at the end.
  let run: ToolCallPair[] = [];
  const flush = () => {
    if (run.length === 0) return;
    const group = run;
    run = [];
    out.push(<ToolGroup key={`tools-${group[0].call.seq}`} pairs={group} compact={compact} />);
  };

  // Both of these track the *previous* occurrence of their entry kind as the
  // walk proceeds, because neither line means anything on its own:
  //
  // - `lastInit`: the SDK re-emits system/init at the head of every turn in
  //   streaming-input mode, not once per session. Rendering each one as
  //   "session started" put a false session boundary between every prompt
  //   and its answer. Only a *changed* init is news.
  // - `lastResult`: a result's cost/api-time fields are session-cumulative,
  //   so each line needs the one before it to show its own share.
  //
  // A changed init means the SDK really did re-initialise, so the result
  // counters restart with it — that's why lastResult resets there.
  let lastInit: string | null = null;
  let lastResult: SdkResultSummary | null = null;

  for (let idx = 0; idx < entries.length; idx++) {
    const entry = entries[idx];
    const key = String(entry.seq);

    // Rendered by their partner entry, never on their own.
    if (entry.type === TranscriptEntryType.ANSWER) continue;
    if (entry.type === TranscriptEntryType.PERMISSION_RESPONSE) continue;
    if (entry.type === TranscriptEntryType.USER && consumedResults.has(entry.seq)) continue;
    // TodoWrite results: the call is already filtered (transcript.ts L303), so
    // its result isn't in consumedResults. Filter using precomputed set.
    if (entry.type === TranscriptEntryType.USER) {
      const result = parseSdkToolResult(entry.text);
      if (result?.toolUseId && todoWriteToolUseIds.has(result.toolUseId)) {
        continue;
      }
    }

    // --- tier 1: decisions ---------------------------------------------------
    if (entry.type === TranscriptEntryType.PERMISSION_REQUEST) {
      let parsed: { tool?: unknown; input?: unknown };
      try {
        parsed = JSON.parse(entry.text);
      } catch {
        continue;
      }
      if (typeof parsed.tool !== "string") continue;
      const isPending = pendingSeqs.has(entry.seq);
      if (isPending && dockPendingDecision) continue;
      flush();
      const permissionKey = `permission:${entry.seq}`;
      const respond = (decision: { behavior: "allow" | "deny"; message?: string }) => onRespond(entry.seq, decision);

      if (parsed.tool === "ExitPlanMode") {
        const plan = (parsed.input as { plan?: string } | undefined)?.plan;
        if (typeof plan !== "string") continue;
        out.push(
          <div key={key} id={`entry-${key}`}>
            <PlanCard
              plan={plan}
              pending={isPending}
              busy={busyKey === permissionKey}
              decision={decisions.get(entry.seq)}
              onApprove={(mode) => onApprovePlan(entry.seq, mode)}
              onFeedback={onPlanFeedback}
              edgeClassName={edge}
            />
          </div>,
        );
        continue;
      }
      out.push(
        <div key={key} id={`entry-${key}`}>
          <PermissionCard
            tool={parsed.tool}
            input={parsed.input}
            pending={isPending}
            busy={busyKey === permissionKey}
            decision={decisions.get(entry.seq)}
            denyMessage={denyMessages.get(entry.seq)}
            onAllow={() => respond({ behavior: "allow" })}
            onDeny={(message) => respond({ behavior: "deny", message })}
            edgeClassName={edge}
          />
        </div>,
      );
      continue;
    }

    if (entry.type === TranscriptEntryType.QUESTION) {
      const answerEntry = entries.slice(idx + 1).find((e) => e.type === TranscriptEntryType.ANSWER);
      if (!answerEntry && dockPendingDecision) continue;
      flush();
      out.push(
        <div key={key} id={`entry-${key}`}>
          <QuestionCard
            entry={entry}
            answer={answerEntry ? parseAnswers(answerEntry.text) : null}
            busy={busyKey === `question:${entry.seq}`}
            compact={compact}
            onSubmit={(answers) => onAnswer(entry.seq, answers)}
          />
        </div>,
      );
      continue;
    }

    // --- tier 5: alarms (never gated) ----------------------------------------
    if (entry.type === TranscriptEntryType.SYSTEM) {
      const info = parseSdkSystemInfo(entry.text);
      if (info) {
        const broken = (info.mcpServers ?? []).filter((s) => s.status !== "connected");
        if (broken.length > 0) {
          flush();
          out.push(
            <div key={`${key}-mcp`} id={`entry-${key}`}>
              {broken.map((s) => (
                <AlarmBar
                  key={s.name}
                  compact={compact}
                  text={
                    <>
                      mcp server <span className="font-semibold">{s.name}</span> {s.status} — its tools are missing from
                      this session
                    </>
                  }
                />
              ))}
            </div>,
          );
        }
        // The rest of session-init is tier 4 — and only when it differs
        // from the last one shown. The alarm above deliberately stays
        // outside this check: a broken MCP server should shout on every
        // turn it is still broken.
        const fingerprint = [
          info.model,
          info.permissionMode,
          info.tools?.length,
          (info.mcpServers ?? []).map((s) => `${s.name}:${s.status}`).join(","),
        ].join("|");
        const changed = fingerprint !== lastInit;
        lastInit = fingerprint;
        if (changed) lastResult = null;
        if (visibility.quiet && changed) {
          out.push(
            <div key={key}>
              <TranscriptEntryView entry={entry} compact={compact} />
            </div>,
          );
        }
        continue;
      }

      const sig = parseSdkSignal(entry.text);
      const isAlarm = Boolean(sig?.error) || (sig?.sdk === "auth_status" && sig.status !== "ok" && Boolean(sig?.status));
      if (isAlarm && sig) {
        flush();
        out.push(
          <div key={key} id={`entry-${key}`}>
            <AlarmBar compact={compact} text={sig.sdk.replace(/_/g, " ")} detail={sig.error ?? sig.status ?? null} />
          </div>,
        );
        continue;
      }
      // tool_progress and friends are tier 4.
      if (!visibility.quiet) continue;
      out.push(
        <div key={key}>
          <TranscriptEntryView entry={entry} compact={compact} />
        </div>,
      );
      continue;
    }

    // --- tier 3: tool activity ----------------------------------------------
    if (entry.type === TranscriptEntryType.ASSISTANT) {
      const pair = pairsBySeq.get(entry.seq);
      if (pair) {
        if (visibility.tools) run.push(pair);
        continue;
      }
      const info = parseSdkToolUse(entry.text);
      // A thinking block is tier 2 (the agent's reasoning), collapsed by
      // default — it is not a tool call, so the tools toggle doesn't hide it.
      if (info?.kind === "thinking" && info.text) {
        if (!visibility.narrative) continue;
        flush();
        out.push(
          <div key={key} id={`entry-${key}`} className={compact ? "pl-4" : "pl-[18px]"}>
            <Collapse
              className="border-0 bg-transparent"
              summaryClassName="px-0 py-0 bg-transparent hover:bg-transparent"
              contentClassName="px-0 pt-1.5"
              summary={
                <span className="text-xs text-dim2 hover:text-dim">
                  ▸ thinking{info.text.length > 0 ? ` · ${Math.max(1, Math.round(info.text.length / 4 / 100) / 10)}k tokens` : ""}
                </span>
              }
            >
              <div className="text-xs leading-[1.7] text-dim whitespace-pre-wrap">{info.text}</div>
            </Collapse>
          </div>,
        );
        continue;
      }
      // A TodoWrite call: the TODOS panel owns it, nothing to render here.
      if (info?.tool === "TodoWrite") continue;
      continue;
    }

    // The sidecar's periodic {branch, files[]} telemetry. The CHANGES panel
    // summarises it regardless, so in the feed it's tier 4 — a one-line rule.
    // TranscriptEntryView has no branch for this type and would fall through to
    // the prose renderer, dumping the raw JSON payload into the conversation.
    if (entry.type === TranscriptEntryType.TOOL_CALL) {
      if (!visibility.quiet) continue;
      flush();
      out.push(
        <div key={key}>
          <ToolCallLine entry={entry} />
        </div>,
      );
      continue;
    }

    // --- tier 4: lifecycle ---------------------------------------------------
    // A result carries session-cumulative cost/api-time, so it renders as a
    // delta against the previous one. The baseline advances outside the
    // visibility gate: it describes the session, not what is on screen.
    if (entry.type === TranscriptEntryType.RESULT) {
      const summary = parseSdkResultSummary(entry.text);
      const prev = lastResult;
      if (summary) lastResult = summary;
      if (!visibility.quiet) continue;
      flush();
      out.push(
        <div key={key} id={`entry-${key}`}>
          <TranscriptEntryView
            entry={entry}
            compact={compact}
            result={summary ? resultDelta(summary, prev) : null}
          />
        </div>,
      );
      continue;
    }

    if (
      entry.type === TranscriptEntryType.APPROVE ||
      entry.type === TranscriptEntryType.ABORT ||
      entry.type === TranscriptEntryType.INTERRUPT ||
      entry.type === TranscriptEntryType.PERMISSION_MODE
    ) {
      if (!visibility.quiet) continue;
      flush();
      out.push(
        <div key={key} id={`entry-${key}`}>
          <TranscriptEntryView entry={entry} compact={compact} />
        </div>,
      );
      continue;
    }

    // --- tier 2: narrative ---------------------------------------------------
    if (!visibility.narrative) continue;
    flush();

    // Gated on the AUTHOR, not on the text: core is the only writer of
    // from === "session", so matching the prefix alone rendered a HUMAN message
    // that happened to start with the literal `[from session x]` as a
    // cross-session prompt. `!== null` rather than truthiness — a session entry
    // whose prefix is missing is still a peer message, not agent prose.
    const peer = entry.from === "session" ? (CROSS_SESSION.exec(entry.text)?.[1] ?? "") : null;
    if (peer !== null) {
      out.push(
        <div key={key} id={`entry-${key}`} className="flex gap-2.5 items-baseline">
          <span className="text-green-soft text-xs sm:text-sm flex-none">↘</span>
          <div className={`text-dim leading-[1.65] min-w-0 ${compact ? "text-xs" : "text-sm"}`}>
            from <span className="text-text2">{peer ? `#${peer.slice(0, 6)}` : "another session"}</span> —{" "}
            {entry.text.replace(CROSS_SESSION, "")}
          </div>
        </div>,
      );
      continue;
    }

    if (entry.from === "human") {
      out.push(
        <div key={key} id={`entry-${key}`} className="flex gap-2.5 items-baseline">
          <span className="text-primary flex-none text-sm sm:text-base">❯</span>
          <div className={`text-text2 leading-[1.7] min-w-0 ${compact ? "text-sm" : "text-base"}`}>
            {entry.text}
          </div>
        </div>,
      );
      continue;
    }

    out.push(
      <div key={key} id={`entry-${key}`} className="flex gap-2.5">
        <span className="w-1.5 h-1.5 rounded-full bg-warning mt-[7px] flex-none" />
        <div
          className={`leading-[1.7] min-w-0 ${compact ? "text-sm" : "text-base max-w-[760px]"}`}
          title={entryTime(entry) ?? undefined}
        >
          <Markdown text={asDisplayMarkdown(entry)} />
        </div>
      </div>,
    );
  }
  flush();

  if (out.length === 0) {
    return (
      <div className="text-sm text-dim2">
        {density === "everything"
          ? "Nothing in this session yet."
          : "Nothing at this density — switch back to see the full feed."}
      </div>
    );
  }

  return <>{out}</>;
}

export { QuietRule };
