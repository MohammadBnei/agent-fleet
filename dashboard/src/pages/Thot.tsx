import { useCallback, useEffect, useState } from "react";
import { client } from "../connectClient";
import { PermissionCard } from "../components/PermissionCard";
import type { ThotEvent } from "../gen/agentfleet/v1/dashboard_pb";

// thot's activity feed + the human side of its permission prompts
// (docs/adr/0035). This is the ONLY surface where a thot permission
// decision can be made — its Discord channel is notify-only by design, so
// there's deliberately no second path a decision could arrive through.
//
// ponytail: a plain 2s poll rather than reusing the task feed's SSE
// stream. thot's event rate is orders of magnitude lower than a live
// task transcript's (an audit or alert every few minutes at most), and a
// poll is a fraction of the machinery. Swap to the streaming hub if the
// feed ever gets chatty enough to notice.
const POLL_MS = 2000;

function kindBadge(kind: string): string {
  switch (kind) {
    case "alert":
      return "badge-error";
    case "finding":
      return "badge-info";
    case "audit_run":
      return "badge-ghost";
    case "permission_response":
      return "badge-success";
    default:
      return "badge-warning";
  }
}

function parseToolName(payload: string): { tool: string; input: unknown } {
  try {
    const parsed = JSON.parse(payload);
    return { tool: parsed.tool ?? "unknown", input: parsed.input ?? parsed };
  } catch {
    return { tool: "unknown", input: payload };
  }
}

export function Thot() {
  const [events, setEvents] = useState<ThotEvent[]>([]);
  const [pending, setPending] = useState<ThotEvent[]>([]);
  const [busyId, setBusyId] = useState<bigint | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [question, setQuestion] = useState("");
  const [asking, setAsking] = useState(false);

  const load = useCallback(async () => {
    try {
      // sinceId 0 every time: the feed is small and capped by `limit`, so
      // re-reading it is cheaper than tracking a cursor and reconciling
      // it against the separately-returned pending list.
      const res = await client.listThotEvents({ sinceId: 0n, limit: 200 });
      setEvents(res.events);
      setPending(res.pending);
      setError(null);
    } catch (err) {
      setError(String(err));
    }
  }, []);

  useEffect(() => {
    void load();
    const t = setInterval(() => void load(), POLL_MS);
    return () => clearInterval(t);
  }, [load]);

  async function ask() {
    const q = question.trim();
    if (!q || asking) return;
    setAsking(true);
    setError(null);
    try {
      // The answer isn't rendered from this response — both sides are
      // recorded in thot_events, so the poll below picks them up and the
      // feed stays the single source of truth for what was said.
      await client.askThot({ question: q });
      setQuestion("");
      await load();
    } catch (err) {
      setError(String(err));
    } finally {
      setAsking(false);
    }
  }

  async function respond(requestId: bigint, allow: boolean, message: string) {
    setBusyId(requestId);
    try {
      await client.respondToThotPermission({ requestId, allow, message });
      await load();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusyId(null);
    }
  }

  return (
    <div className="px-4 py-4 max-w-4xl mx-auto">
      <div className="flex items-baseline gap-3 mb-4">
        <h2 className="text-lg font-semibold">thot</h2>
        <span className="text-[11px] text-base-content/50">cluster agent activity</span>
      </div>

      {error && <p className="text-error text-[12px] mb-3">{error}</p>}

      <div className="flex items-start gap-2 mb-6">
        <textarea
          value={question}
          onChange={(e) => setQuestion(e.target.value)}
          onKeyDown={(e) => {
            // Enter sends, shift+Enter newlines — same convention as the
            // task detail composer.
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void ask();
            }
          }}
          rows={2}
          disabled={asking}
          placeholder="Ask thot about the cluster… (reads run without asking; anything mutating will prompt below)"
          className="flex-1 bg-transparent border border-base-content/15 rounded-lg px-3 py-2 text-[12px] outline-none focus:border-primary/50 resize-y"
        />
        <button
          type="button"
          className="btn btn-primary btn-sm"
          disabled={asking || question.trim() === ""}
          onClick={() => void ask()}
        >
          {asking ? <span className="loading loading-spinner loading-xs"></span> : "Ask"}
        </button>
      </div>

      {asking && (
        <p className="text-[11px] text-base-content/50 mb-4">
          thot is working — if it needs to run something mutating, a permission card
          will appear below and it will wait for you.
        </p>
      )}

      {pending.length > 0 && (
        <div className="mb-6">
          <div className="text-[10px] tracking-[0.12em] font-semibold text-primary mb-2">
            AWAITING YOUR DECISION
          </div>
          {pending.map((p) => {
            const { tool, input } = parseToolName(p.payload);
            return (
              <PermissionCard
                key={String(p.id)}
                tool={tool}
                input={input}
                pending
                busy={busyId === p.id}
                onAllow={() => void respond(p.id, true, "")}
                onDeny={(message) => void respond(p.id, false, message)}
                edgeClassName="-mx-4 px-4"
              />
            );
          })}
        </div>
      )}

      {events.length === 0 && !error && (
        <p className="text-[12px] text-base-content/50">No activity yet.</p>
      )}

      <div className="flex flex-col gap-2">
        {events
          .slice()
          .reverse()
          .map((e) => (
            <div key={String(e.id)} className="flex items-start gap-2 text-[12px]">
              <span className={`badge badge-xs flex-none ${kindBadge(e.kind)}`}>{e.kind}</span>
              <span className="text-base-content/40 flex-none text-[10.5px]">{e.actor}</span>
              <pre className="flex-1 min-w-0 whitespace-pre-wrap break-words text-base-content/80">
                {e.payload}
              </pre>
              <span className="text-base-content/30 flex-none text-[10px]">{e.createdAt}</span>
            </div>
          ))}
      </div>
    </div>
  );
}
