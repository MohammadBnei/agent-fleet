import { useEffect, useState } from "react";
import { client } from "../connectClient";
import type { ModelStat } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "./InlineError";

// ListSessionRatings is read once per session and shared by every thumb in the
// feed; a rate invalidates the entry so the next mount reads the stored value.
const ratingsCache = new Map<
  string,
  Promise<{
    thumbs: { seq: bigint; value: number }[];
    score: number;
    note: string;
  }>
>();
function storedRatings(sessionId: string) {
  let p = ratingsCache.get(sessionId);
  if (!p) {
    p = client.listSessionRatings({ sessionId });
    ratingsCache.set(sessionId, p);
    p.catch(() => ratingsCache.delete(sessionId));
  }
  return p;
}

// Thumbs on a result line. RateTurn has no "clear", so the active thumb
// clicked again just re-sends it.
export function TurnRating({
  sessionId,
  seq,
}: {
  sessionId: string;
  seq: bigint;
}) {
  const [value, setValue] = useState<1 | -1 | 0>(0);
  const [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    storedRatings(sessionId)
      .then((r) => {
        const t = r.thumbs.find((x) => x.seq === seq);
        if (live && t) setValue(t.value === 1 ? 1 : -1);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [sessionId, seq]);
  function rate(v: 1 | -1) {
    setError("");
    client
      .rateTurn({ sessionId, seq, value: v, note: "" })
      .then(() => {
        ratingsCache.delete(sessionId);
        setValue(v);
      })
      .catch((e: Error) => setError(e.message));
  }
  const cls = (on: boolean) =>
    `px-1 cursor-pointer ${on ? "text-primary" : "text-dim2 hover:text-dim"}`;
  return (
    <span className="inline-flex items-center gap-1 text-xs">
      <button
        type="button"
        aria-label="good turn"
        aria-pressed={value === 1}
        className={cls(value === 1)}
        onClick={() => rate(1)}
      >
        ▲
      </button>
      <button
        type="button"
        aria-label="bad turn"
        aria-pressed={value === -1}
        className={cls(value === -1)}
        onClick={() => rate(-1)}
      >
        ▼
      </button>
      {error && <span className="text-error">{error}</span>}
    </span>
  );
}

// 1-5 plus an optional note for a finished session. Sending again edits it.
export function SessionScore({ sessionId }: { sessionId: string }) {
  const [score, setScore] = useState(0);
  const [note, setNote] = useState("");
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    storedRatings(sessionId)
      .then((r) => {
        if (!live || r.score === 0) return;
        setScore(r.score);
        setNote(r.note);
        setSaved(true);
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [sessionId]);
  function send(next: number, text: string) {
    setError("");
    client
      .rateSession({ sessionId, score: next, note: text })
      .then(() => {
        ratingsCache.delete(sessionId);
        setScore(next);
        setSaved(true);
      })
      .catch((e: Error) => setError(e.message));
  }
  return (
    <div className="mt-2.5 text-xs flex flex-col gap-1">
      <div className="flex items-center gap-1.5">
        <span className="text-dim2">rate</span>
        {[1, 2, 3, 4, 5].map((n) => (
          <button
            key={n}
            type="button"
            aria-pressed={score === n}
            className={`w-5 border cursor-pointer ${score === n ? "border-primary text-primary" : "border-line text-dim hover:text-text2"}`}
            onClick={() => send(n, note)}
          >
            {n}
          </button>
        ))}
      </div>
      {score > 0 && (
        <input
          className="input input-xs w-full"
          placeholder="note (optional)"
          value={note}
          onChange={(e) => {
            setNote(e.target.value);
            setSaved(false);
          }}
          onBlur={() => !saved && send(score, note)}
        />
      )}
      {error && <span className="text-error">{error}</span>}
    </div>
  );
}

const usd = (n: number) => `$${n.toFixed(2)}`;

// 12,000 -> 12k, 1,250,000 -> 1.3M: the full figure never fits a phone.
export function compact(n: bigint): string {
  const v = Number(n);
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)}M`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(v >= 1e5 ? 0 : 1)}k`;
  return String(v);
}

// The vendor prefix says nothing the route column does not; the title keeps the id.
export function shortModel(id: string): string {
  return id.replace(/^(claude|zai-org|z-ai)-/, "");
}

// A session is scored once, as a whole, and the score is filed under the model
// that did most of its work; a session that switched models is not split.
export const SCORE_NOTE =
  "A session score counts toward the session's most-used model.";

export const STAT_HEAD = [
  "model",
  "route",
  "sessions",
  "tokens in/out",
  "billed",
  "list",
  "rated",
  "avg score",
  "errors",
];

export function statRow(s: ModelStat): string[] {
  return [
    shortModel(s.model),
    s.route,
    String(s.sessions),
    `${compact(s.inputTokens)} / ${compact(s.outputTokens)}`,
    usd(s.billedUsd),
    usd(s.listUsd),
    `${s.thumbsUp} up / ${s.thumbsDown} down`,
    s.scores > 0n ? s.avgScore.toFixed(1) : "—",
    String(s.errorResults),
  ];
}

// A table from md up, one stacked card per model below it: the same rows
// either way, so the two cannot drift apart.
export function ModelStatsTable({ stats }: { stats: ModelStat[] }) {
  if (stats.length === 0)
    return <div className="text-xs text-dim2">No sessions in this window.</div>;
  return (
    <>
      <div className="hidden md:block">
        <table className="table table-xs w-full">
          <thead>
            <tr>
              {STAT_HEAD.map((h) => (
                <th key={h} title={h === "avg score" ? SCORE_NOTE : undefined}>
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {stats.map((s) => (
              <tr key={`${s.model}/${s.route}`}>
                {statRow(s).map((c, i) => (
                  <td
                    key={STAT_HEAD[i]}
                    title={i === 0 ? s.model : undefined}
                    className="whitespace-nowrap"
                  >
                    {c}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="flex flex-col gap-2 md:hidden">
        {stats.map((s) => (
          <dl
            key={`${s.model}/${s.route}`}
            className="border border-line p-2 text-xs grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5"
          >
            {statRow(s).map((c, i) => (
              <div key={STAT_HEAD[i]} className="contents">
                <dt className="text-dim2">{STAT_HEAD[i]}</dt>
                <dd
                  className={i === 0 ? "font-semibold" : undefined}
                  title={i === 0 ? s.model : undefined}
                >
                  {c}
                </dd>
              </div>
            ))}
          </dl>
        ))}
      </div>
    </>
  );
}

export function ModelStatsPanel() {
  const [stats, setStats] = useState<ModelStat[] | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    client
      .listModelStats({ days: 7 })
      .then((r) => setStats(r.stats))
      .catch((e: Error) => setError(e.message));
  }, []);
  return (
    <div className="flex flex-col gap-3">
      <h3 className="text-sm font-semibold">Models, last 7 days</h3>
      {error && <InlineError message={error} />}
      {stats && stats.length > 0 && (
        <p className="text-xs text-dim2">{SCORE_NOTE}</p>
      )}
      {stats && <ModelStatsTable stats={stats} />}
    </div>
  );
}
