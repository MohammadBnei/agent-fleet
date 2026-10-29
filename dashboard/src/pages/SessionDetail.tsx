import { useState } from "react";
import { AutoModeConfirm } from "../components/AutoModeConfirm";
import { Composer } from "../components/Composer";
import { DecisionDock } from "../components/DecisionDock";
import { AgentDetailModal } from "../components/DetailModal";
import { ErrorModal } from "../components/ErrorModal";
import { FileDiffModal } from "../components/FileDiffModal";
import { Markdown } from "../components/Markdown";
import { Segmented } from "../components/Segmented";
import { SessionFeed } from "../components/SessionFeed";
import {
  AgentsPanel,
  BackgroundPanel,
  ChangesPanel,
  SessionPanel,
  TodosPanel,
} from "../components/SessionPanels";
import { client } from "../connectClient";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { sessionLabel } from "../sessionLabel";
import { isPodPhaseLive, sessionBadge } from "../sessionState";
import type { SubagentRun } from "../transcript";
import {
  asDisplayMarkdown,
  type Density,
  feedVisibility,
  latestResultSummary,
} from "../transcript";
import { useLocalStorageState } from "../useLocalStorageState";
import { useSessionDetail } from "../useSessionDetail";
import {
  sessionPanelData,
  useComposer,
  useFeedScroll,
} from "../useSessionView";

// The console's desktop session view: feed · decision dock · composer, beside a
// fixed panel column. Full-width — the permanent 320px session-list sidebar is
// gone, and the rich list view is the fleet overview it used to stand in for.
//
// The pending decision lives in the dock and nowhere else (docs/adr/0043): it
// used to render inline in the feed *and* as a spine item *and* as an answer
// chip above the composer. `dockPendingDecision` is what keeps the feed from
// rendering it a second time.

const DENSITY: readonly { value: Density; label: string; title: string }[] = [
  {
    value: "everything",
    label: "everything",
    title: "every entry, including tool calls and lifecycle lines",
  },
  {
    value: "narrative",
    label: "narrative",
    title: "the readable conversation only",
  },
  {
    value: "decisions",
    label: "decisions",
    title: "decisions and alarms only",
  },
];

export function SessionDetail({
  sessionId,
  sessions,
}: {
  sessionId: string;
  sessions: Session[];
}) {
  const {
    session,
    entries,
    busyKey,
    loadError,
    actionError,
    pendingMessage,
    hasOlder,
    loadingOlder,
    loadOlder,
    run,
    sendDiscuss,
    respondToPermission,
    previewGrant,
    approvePlanDecision,
    answerQuestion,
    clearActionError,
  } = useSessionDetail(
    sessionId,
    sessions.find((t) => t.id === sessionId),
  );
  const { message, setMessage, slashCommands, sendMessage } = useComposer(
    entries,
    sendDiscuss,
  );
  const [autoOpen, setAutoOpen] = useState(false);
  // The CHANGES row a human clicked, or null. See FileDiffModal.
  const [diffPath, setDiffPath] = useState<string | null>(null);
  // The AGENTS row a human tapped, or null. See DetailModal.
  const [agentDetail, setAgentDetail] = useState<SubagentRun | null>(null);
  // Key deliberately not renamed with the file: it is persisted in every
  // operator's browser, and renaming it silently resets their density choice.
  const [density, setDensity] = useLocalStorageState<Density>(
    "taskDetail.density",
    "everything",
  );
  const {
    ref: feedRef,
    atBottom: feedAtBottom,
    onScroll: feedOnScroll,
    scrollToBottom: feedScrollToBottom,
    anchorPrepend,
    hasNew: hasNewAiMessage,
  } = useFeedScroll(sessionId, entries, pendingMessage);

  if (loadError)
    return (
      <div className="m-4 border border-pink-line bg-pink-bg px-4 py-3 text-base text-error">
        {loadError}
      </div>
    );
  // `session` is App's polled list copy when the list has it, and
  // useSessionDetail's own poll when it does not — live either way.
  if (!session) return <div className="p-4 text-base text-dim">Loading…</div>;

  const badge = sessionBadge(session);
  // staleTag/heartbeat used to render "last beat 4m ago", reddened past the
  // reclaim threshold. Both are gone with heartbeat_at (docs/adr/0048) — the
  // feed's own entry timestamps say when this session last did anything, and
  // they say it about real work rather than about a timer.
  const blocked = session.liveState === "blocked";
  const visibility = feedVisibility(density, false);

  const { todos, branch, changes, agents, background, model } =
    sessionPanelData(entries);
  const result = latestResultSummary(entries);
  const contextTokens =
    (result?.usage?.input_tokens ?? 0) +
    (result?.usage?.cache_read_input_tokens ?? 0) +
    (result?.usage?.cache_creation_input_tokens ?? 0);

  return (
    <div className="flex-1 min-h-0 flex">
      <div className="flex-1 min-w-0 flex flex-col border-r border-line">
        <div className="flex-none flex items-center gap-3 px-4.5 py-3 border-b border-line flex-wrap">
          {/* The "← all sessions" control is gone. Back is the header's "herd"
              logo — a real <a href="/"> that resets the SPA to the list — so the
              title row does not carry its own, and the title starts the row. */}
          {/* No id — see the note in MobileSessionDetail. It is in the SESSION
              panel with the other facts. */}
          <h2 className="text-lg font-semibold min-w-0 break-words">
            {sessionLabel(session)}
          </h2>
          {blocked ? (
            <span className="flex items-center gap-1.5 border border-pink-line bg-pink-chip px-2 py-0.5 flex-none">
              <span className="w-1.5 h-1.5 rounded-full bg-error animate-fpulse" />
              <span className="text-xs font-medium text-error">blocked</span>
            </span>
          ) : (
            badge && (
              <span
                className={`text-xs px-1.5 py-px border tracking-wide flex-none ${badge.className}`}
                title={badge.title ?? session.podMessage ?? undefined}
              >
                {badge.label}
              </span>
            )
          )}

          <span className="text-xs text-dim2 min-w-0 truncate">
            {session.repo}
            {branch && ` · ${branch}`}
          </span>

          <div className="ml-auto flex items-center gap-2 flex-none">
            <span className="text-2xs tracking-[0.1em] text-dim2">DENSITY</span>
            <Segmented
              value={density}
              options={DENSITY}
              onChange={setDensity}
            />
          </div>
        </div>

        <div className="relative flex-1 min-h-0">
          <div
            ref={feedRef}
            onScroll={feedOnScroll}
            className="absolute inset-0 overflow-y-auto px-4.5 py-4.5 flex flex-col gap-4"
          >
            <SessionFeed
              hasPendingMessage={pendingMessage !== null}
              entries={entries}
              visibility={visibility}
              density={density}
              busyKey={busyKey}
              hasOlder={hasOlder}
              loadingOlder={loadingOlder}
              onLoadOlder={() => anchorPrepend(loadOlder)}
              dockPendingDecision
              onRespond={respondToPermission}
              previewGrant={previewGrant}
              onApprovePlan={approvePlanDecision}
              onAnswer={answerQuestion}
              onPlanFeedback={(text) => sendDiscuss(text)}
            />
            {pendingMessage && (
              <div className="flex gap-2.5 items-baseline opacity-60">
                <span className="text-primary flex-none text-base">❯</span>
                <div className="flex-1 min-w-0 text-base leading-[1.7] text-text2">
                  <Markdown
                    text={asDisplayMarkdown({
                      from: "human",
                      text: pendingMessage,
                    })}
                  />
                </div>
                <span className="loading loading-spinner loading-xs flex-none" />
              </div>
            )}
          </div>
          {!feedAtBottom && (
            <button
              type="button"
              onClick={feedScrollToBottom}
              className="absolute bottom-3 left-1/2 -translate-x-1/2 border border-line bg-base-300 px-2 py-1 text-sm shadow-md"
              title="Scroll to bottom"
            >
              ↓
              {hasNewAiMessage && (
                <span className="absolute -top-1 -right-1 w-1.5 h-1.5 rounded-full bg-error animate-fpulse" />
              )}
            </button>
          )}
        </div>

        <DecisionDock
          entries={entries}
          podLive={isPodPhaseLive(session.podPhase)}
          swept={session.sweptAt !== undefined}
          busyKey={busyKey}
          onRespond={respondToPermission}
          previewGrant={previewGrant}
          onApprovePlan={approvePlanDecision}
          onAnswer={answerQuestion}
          onPlanFeedback={(text) => sendDiscuss(text)}
        />

        <div className="flex-none px-4.5 py-3 border-t border-line">
          <ErrorModal message={actionError} onClose={clearActionError} />
          <FileDiffModal
            sessionId={sessionId}
            path={diffPath}
            entries={entries}
            onClose={() => setDiffPath(null)}
          />
          <AgentDetailModal
            run={agentDetail}
            onClose={() => setAgentDetail(null)}
          />
          <AutoModeConfirm
            open={autoOpen}
            onCancel={() => setAutoOpen(false)}
            onConfirm={() => {
              setAutoOpen(false);
              run(
                () => client.setPermissionMode({ sessionId, mode: "auto" }),
                "action:mode",
              );
            }}
          />

          {slashCommands.length > 0 && (
            <div className="flex gap-2 mb-2.5 flex-wrap">
              {slashCommands.map((c) => (
                <button
                  key={c}
                  type="button"
                  onClick={() => setMessage(`/${c} `)}
                  className="border border-line text-dim px-3 py-1 text-xs cursor-pointer hover:text-base-content"
                >
                  /{c}
                </button>
              ))}
            </div>
          )}

          <Composer
            value={message}
            onChange={setMessage}
            onSend={sendMessage}
            disabled={busyKey !== null}
            placeholder="message the agent — / for commands"
          />

          {/* The permission mode used to sit here as well as in the SESSION
              panel, where it is also a control rather than a restatement. What
              this line is for is the thing nothing else answers from the
              composer: which branch the agent is committing to. */}
          <div className="flex gap-3.5 mt-2 text-xs text-dim2 flex-wrap">
            <span
              className={branch ? "text-text2" : undefined}
              title="the branch the agent is working on"
            >
              ⑂ {branch ?? "no branch yet"}
            </span>
            {/* contextTokens > 0: a result without usage rendered "ctx 0k". */}
            {result && contextTokens > 0 && (
              // Per-turn, not cumulative: the SDK reports usage per result and
              // nothing sums it, so labelling this a session total would be a
              // lie. See docs/dashboard-spec.md — no cost/token rollup exists.
              <span title="context used by the most recent turn — there is no session-level rollup on the wire">
                ctx {Math.round(contextTokens / 1000)}k last turn
              </span>
            )}
          </div>
        </div>
      </div>

      <div className="w-[266px] flex-none overflow-y-auto px-3.5 py-3.5 flex flex-col gap-4.5 min-w-0">
        <TodosPanel todos={todos} blocked={blocked} />
        <ChangesPanel
          branch={branch}
          changes={changes}
          onOpenFile={setDiffPath}
        />
        <AgentsPanel runs={agents} onOpenAgent={setAgentDetail} />
        <BackgroundPanel tasks={background} />
        <div className="mt-auto">
          <SessionPanel
            model={model}
            session={session}
            busy={busyKey !== null}
            busyKey={busyKey}
            run={run}
            onAutoClick={() => setAutoOpen(true)}
            onRestart={() => sendDiscuss("/clear")}
          />
        </div>
      </div>
    </div>
  );
}
