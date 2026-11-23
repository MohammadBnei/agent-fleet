import { useEffect, useState } from "react";
import { client } from "../connectClient";
import type { ModelStat } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "./InlineError";

// Local state only: the API has no read-back, so "current" is what this tab
// last sent. A reload shows neutral; a click overwrites the stored rating.

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
  function rate(v: 1 | -1) {
    setError("");
    client
      .rateTurn({ sessionId, seq, value: v, note: "" })
      .then(() => setValue(v))
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
  function send(next: number, text: string) {
    setError("");
    client
      .rateSession({ sessionId, score: next, note: text })
      .then(() => {
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
const tok = (n: bigint) => Number(n).toLocaleString();

export const STAT_HEAD = [
  "model",
  "route",
  "sessions",
  "tokens in",
  "tokens out",
  "billed",
  "up",
  "down",
  "avg score",
  "errors",
];

export function statRow(s: ModelStat): string[] {
  return [
    s.model,
    s.route,
    String(s.sessions),
    tok(s.inputTokens),
    tok(s.outputTokens),
    usd(s.billedUsd),
    String(s.thumbsUp),
    String(s.thumbsDown),
    s.scores > 0n ? s.avgScore.toFixed(1) : "—",
    String(s.errorResults),
  ];
}

export function ModelStatsTable({ stats }: { stats: ModelStat[] }) {
  if (stats.length === 0)
    return <div className="text-xs text-dim2">No sessions in this window.</div>;
  return (
    <div className="overflow-x-auto">
      <table className="table table-xs">
        <thead>
          <tr>
            {STAT_HEAD.map((h) => (
              <th key={h}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {stats.map((s) => (
            <tr key={`${s.model}/${s.route}`}>
              {statRow(s).map((c, i) => (
                <td key={STAT_HEAD[i]}>{c}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
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
      {stats && <ModelStatsTable stats={stats} />}
    </div>
  );
}
