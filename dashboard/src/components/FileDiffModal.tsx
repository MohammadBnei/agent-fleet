import { useEffect, useState } from "react";
import { Modal } from "./Modal";
import { ToolInputView } from "./ToolInputView";
import { DiffLines } from "./DiffLines";
import type { Line } from "./ToolInputView";
import { fileEdits } from "../transcript";
import { client } from "../connectClient";
import { GetFileDiffResponse_Status } from "../gen/agentfleet/v1/dashboard_pb";
import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";

// What the CHANGES panel's `+12 −3` actually was. Same renderer the feed uses
// when you expand an Edit — ToolInputView already turns Edit's old_string/
// new_string and Write's content into a DiffView, so this is a container, not
// a second diff implementation.
//
// Every edit to the file, in order, rather than only the last: a file the agent
// touched five times was five decisions, and the net result hides four of them.
//
// When there is no captured tool input at all — a Bash command wrote the file:
// sed, mv, a codegen script, a formatter — this asks the pod for the real diff
// instead. That is a poll, not a fetch, because core cannot dial the sidecar:
// the request rides out on the sidecar's own 5s telemetry tick and the answer
// rides back on the next one (see core/internal/filediff's package doc).
const POLL_MS = 2000;
// Two full telemetry ticks plus slack. Past this the honest thing is to stop
// and say so — a spinner that never resolves is the failure mode this whole
// path exists to remove, and reproducing it with a nicer animation is not a fix.
const GIVE_UP_MS = 25000;

type Fetched =
  | { state: "loading" }
  | { state: "ready"; diff: string }
  // Terminal, and each says something different: git found nothing for the
  // path (committed, reverted, untracked), the pod is gone so nothing can ever
  // answer, or we waited long enough that something is wrong.
  | { state: "empty" }
  | { state: "noPod" }
  | { state: "timeout" }
  | { state: "error"; message: string };

function useLiveDiff(sessionId: string, path: string | null, enabled: boolean): Fetched | null {
  const [fetched, setFetched] = useState<Fetched | null>(null);

  useEffect(() => {
    if (!enabled || !path) {
      setFetched(null);
      return;
    }
    let cancelled = false;
    setFetched({ state: "loading" });
    const startedAt = Date.now();

    const ask = async () => {
      try {
        const res = await client.getFileDiff({ sessionId, path });
        if (cancelled) return;
        switch (res.status) {
          case GetFileDiffResponse_Status.READY:
            setFetched({ state: "ready", diff: res.diff });
            return;
          case GetFileDiffResponse_Status.EMPTY:
            setFetched({ state: "empty" });
            return;
          case GetFileDiffResponse_Status.NO_POD:
            setFetched({ state: "noPod" });
            return;
          default:
            // PENDING: armed, waiting on the pod's next tick.
            if (Date.now() - startedAt > GIVE_UP_MS) setFetched({ state: "timeout" });
            else timer = setTimeout(ask, POLL_MS);
        }
      } catch (err) {
        if (!cancelled) setFetched({ state: "error", message: String(err) });
      }
    };

    let timer: ReturnType<typeof setTimeout> | undefined;
    void ask();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [sessionId, path, enabled]);

  return fetched;
}

// git's unified output -> the Line[] DiffLines already renders. Reusing that
// component rather than adding a second diff renderer is the point: the tinted
// rows, the truncation and the expander are all already built and already what
// the feed shows for an Edit.
//
// The file-level preamble (`diff --git`, `index`, `---`, `+++`) is dropped —
// the modal header already says which file this is, and repeating it as three
// grey rows pushes the actual change below the fold. Hunk headers are kept as
// context: with several hunks, losing them makes distant changes read as
// adjacent ones.
export function unifiedToLines(diff: string): Line[] {
  const out: Line[] = [];
  // The preamble filter applies ONLY before the first hunk header. Past that
  // point every line is file content, and content routinely starts with the
  // same characters: `-- ` opens a comment in SQL and Lua, so a deleted line
  // from this repo's own db/migrations/*.sql was matched by the `--- ` rule
  // and silently dropped — the diff rendered the addition and not the removal
  // it replaced, which is worse than showing nothing.
  let inBody = false;
  for (const raw of diff.split("\n")) {
    if (raw.startsWith("@@")) {
      inBody = true;
      out.push({ kind: "context", text: raw });
      continue;
    }
    if (!inBody) {
      // Header region: `diff --git`, `index`, mode/rename lines, and the
      // `---`/`+++` file pair. The modal header already names the file, and
      // repeating it as three grey rows pushes the change below the fold.
      continue;
    }
    if (raw.startsWith("+")) out.push({ kind: "add", text: raw.slice(1) });
    else if (raw.startsWith("-")) out.push({ kind: "remove", text: raw.slice(1) });
    else if (raw.startsWith(" ")) out.push({ kind: "context", text: raw.slice(1) });
    else if (raw.startsWith("\\")) continue; // "\ No newline at end of file"
    else if (raw !== "") out.push({ kind: "context", text: raw });
  }
  return out;
}

export function FileDiffModal({
  sessionId,
  path,
  entries,
  onClose,
}: {
  sessionId: string;
  // null = closed. Carrying the path rather than a boolean means the modal has
  // nothing to remember when it reopens on a different file.
  path: string | null;
  entries: TranscriptEntry[];
  onClose: () => void;
}) {
  const edits = path ? fileEdits(entries, path) : [];
  // Only ask the pod when the transcript has nothing. A file the agent wrote
  // with Edit/Write is fully answerable offline, and every such open would
  // otherwise cost a poll and up to 10s of latency for a diff already on hand.
  const live = useLiveDiff(sessionId, path, path !== null && edits.length === 0);

  return (
    <Modal open={path !== null} onClose={onClose} boxClassName="max-w-4xl">
      <div className="flex flex-col gap-3">
        <div className="flex items-baseline gap-2 min-w-0">
          <span className="text-2xs tracking-[0.12em] text-dim2 flex-none">DIFF</span>
          <span className="text-sm text-base-content min-w-0 break-all">{path}</span>
          {edits.length > 1 && (
            <span className="text-xs text-dim2 flex-none ml-auto">{edits.length} edits</span>
          )}
          {edits.length === 0 && live?.state === "ready" && (
            // Say where this one came from. It is not the same claim as the
            // replayed-tool-input diff above: this is the working tree as git
            // sees it right now, not a record of a decision the agent made.
            <span className="text-xs text-dim2 flex-none ml-auto">from git</span>
          )}
        </div>
        <div className="max-h-[70vh] overflow-auto flex flex-col gap-4">
          {edits.length === 0 ? (
            <LiveDiff fetched={live} />
          ) : (
            edits.map((e, i) => (
              <div key={String(e.seq)} className="flex flex-col gap-1.5 min-w-0">
                {edits.length > 1 && (
                  <span className="text-2xs tracking-[0.1em] text-dim2">
                    {e.tool.toUpperCase()} {i + 1} OF {edits.length}
                  </span>
                )}
                <ToolInputView tool={e.tool} input={e.input} />
              </div>
            ))
          )}
        </div>
      </div>
    </Modal>
  );
}

// Each terminal state gets its own sentence. The old copy said one thing for
// all of them — "the change did not go through an Edit or Write tool call" —
// which was a guess about the cause presented as fact, and on a torn-down
// session it was the wrong guess.
function LiveDiff({ fetched }: { fetched: Fetched | null }) {
  const note = (text: string) => <div className="text-sm text-dim2 leading-[1.6]">{text}</div>;
  switch (fetched?.state) {
    case "ready":
      // 40 rather than DiffLines' 4-line default: this is the modal a human
      // opened to read a whole file's change, not a preview beside a decision.
      return <DiffLines lines={unifiedToLines(fetched.diff)} maxLines={40} />;
    case "loading":
      return note("Asking the session's pod for this diff… (up to ~10s — it answers on its own telemetry tick)");
    case "empty":
      return note(
        "git reports no change for this file right now. The line counts came from an earlier snapshot — the change has since been committed or reverted.",
      );
    case "noPod":
      return note(
        "This session has no live pod, and the working tree is only readable from inside one. The line counts are the last snapshot its sidecar pushed.",
      );
    case "timeout":
      return note("The session's pod did not answer. It may be busy or shutting down — reopen this file to ask again.");
    case "error":
      return note(`Could not fetch this diff: ${fetched.message}`);
    default:
      return null;
  }
}
