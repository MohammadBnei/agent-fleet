import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";
import { useAtBottom } from "./useAtBottom";
import {
  backgroundTasks,
  latestSlashCommands,
  latestTodos,
  latestToolCallSummary,
  subagentRuns,
} from "./transcript";

// What the desktop SessionDetail and the mobile MobileSessionDetail both do
// with a loaded session, independent of layout. Each view carried its own copy
// of all three, comments included ("same reasoning as SessionDetail's copy"),
// and the copies had already drifted: only desktop knew a new agent message
// had arrived below the fold.

// Feed scrolling.
//
// - A new human entry always jumps to the bottom — the reader wants to see it.
//   A new agent entry follows only if the reader was already at the bottom;
//   otherwise they are reading history, and `hasNew` is raised instead.
//   Keyed on the newest entry's seq, not entries.length: loading older history
//   prepends, which grows the length without anything having arrived.
// - Opening a session lands on the latest message before the first paint
//   (useLayoutEffect), not after it — as a plain effect it painted the top of
//   the history and then visibly scrolled. The view instance is reused across
//   session switches, so it tracks which session it already jumped for.
// - The optimistic echo of a just-sent message scrolls into view immediately,
//   not once the real entry comes back over the stream.
export function useFeedScroll(sessionId: string, entries: TranscriptEntry[], pendingMessage: string | null) {
  const { ref, atBottom, onScroll, scrollToBottom, anchorPrepend } = useAtBottom<HTMLDivElement>();
  const [hasNew, setHasNew] = useState(false);

  const prevLastSeq = useRef<bigint | null>(null);
  useEffect(() => {
    const latest = entries[entries.length - 1];
    const prev = prevLastSeq.current;
    prevLastSeq.current = latest?.seq ?? null;
    if (prev === null || latest === undefined || latest.seq <= prev) return;
    if (latest.from === "human" || atBottom) {
      scrollToBottom();
      setHasNew(false);
    } else {
      setHasNew(true);
    }
  }, [entries, atBottom, scrollToBottom]);

  const scrolledFor = useRef<string | null>(null);
  useLayoutEffect(() => {
    if (entries.length === 0 || scrolledFor.current === sessionId) return;
    scrolledFor.current = sessionId;
    const el = ref.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [sessionId, entries, ref]);

  useEffect(() => {
    if (pendingMessage !== null) scrollToBottom();
  }, [pendingMessage, scrollToBottom]);
  useEffect(() => {
    if (atBottom) setHasNew(false);
  }, [atBottom]);

  return { ref, atBottom, onScroll, scrollToBottom, anchorPrepend, hasNew };
}

// The panel data both views render (TODOS / CHANGES / AGENTS / BACKGROUND).
// Branch and files come from one lookup: the sidecar pushes them in the same
// telemetry snapshot.
export function sessionPanelData(entries: TranscriptEntry[]) {
  const toolSummary = latestToolCallSummary(entries);
  return {
    todos: latestTodos(entries) ?? [],
    branch: toolSummary?.branch ?? null,
    changes: toolSummary?.files ?? null,
    agents: subagentRuns(entries),
    background: backgroundTasks(entries),
  };
}

// The composer's draft, its slash-command suggestions and send. Suggestions
// are name-only (the SDK's init message carries no descriptions or argument
// hints at runtime, docs/adr/0027), shown only while the draft looks like a
// command and filtered by what is typed so far.
export function useComposer(entries: TranscriptEntry[], sendDiscuss: (text: string) => void) {
  const [message, setMessage] = useState("");
  const slashCommands = message.startsWith("/")
    ? (latestSlashCommands(entries) ?? []).filter((c) => c.toLowerCase().startsWith(message.slice(1).toLowerCase()))
    : [];
  function sendMessage() {
    const text = message.trim();
    if (!text) return;
    setMessage("");
    sendDiscuss(text);
  }
  return { message, setMessage, slashCommands, sendMessage };
}
