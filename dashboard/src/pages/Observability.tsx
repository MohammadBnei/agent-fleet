import { useCallback, useState } from "react";
import { client } from "../connectClient";
import { Segmented } from "../components/Segmented";
import { InlineError } from "../components/InlineError";
import { FleetTopology } from "../components/FleetTopology";

type Tab = "topology" | "metrics";

const TABS: readonly { value: Tab; label: string }[] = [
  { value: "topology", label: "topology" },
  { value: "metrics", label: "metrics" },
];

// The queries worth having one click away — each one is a question the
// dashboard's other views genuinely can't answer, which is the same bar
// core's metric set was cut down to (docs/adr/0047).
const PRESETS: readonly { label: string; query: string }[] = [
  // The metric name and its `status` label are core's
  // (core/internal/metrics/metrics.go), so they stay as they are — but there
  // is no queue any more (docs/adr/0048), and publishLiveStateGauge fills that
  // label with live states, not queue statuses. Only the human label is wrong.
  { label: "sessions by live state", query: "sum by (status) (agentfleet_tasks_current)" },
  { label: "headroom", query: "agentfleet_live_pods / agentfleet_max_in_flight" },
  { label: "rpc rate", query: "sum by (surface) (rate(agentfleet_grpc_requests_total[5m]))" },
  {
    label: "rpc errors",
    query: 'sum by (method) (rate(agentfleet_grpc_requests_total{status!~"ok|OK"}[5m]))',
  },
  {
    label: "git p95",
    query:
      "histogram_quantile(0.95, sum by (le, operation) (rate(agentfleet_provisioner_git_operation_duration_seconds_bucket[5m])))",
  },
  { label: "scrape targets up", query: 'up{namespace="agent-fleet"}' },
];

// Prometheus' instant-query result shape. Values arrive as strings on
// purpose — that's how NaN/Inf survive JSON — so they're parsed here rather
// than trusted to be numbers.
type InstantResult = {
  resultType?: string;
  result?: { metric: Record<string, string>; value?: [number, string] }[];
};

// A series' identity minus __name__, which is already the row's first
// column. `{}` for a bare aggregate like sum(...) with no `by`.
function labelsOf(metric: Record<string, string>): string {
  const rest = Object.entries(metric).filter(([k]) => k !== "__name__");
  if (rest.length === 0) return "{}";
  return `{${rest.map(([k, v]) => `${k}="${v}"`).join(", ")}}`;
}

function formatValue(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n)) return raw; // NaN/+Inf — show it, don't launder it
  if (Number.isInteger(n)) return String(n);
  return n.toFixed(Math.abs(n) < 1 ? 4 : 2);
}

function MetricsExplorer() {
  const [query, setQuery] = useState(PRESETS[0].query);
  const [result, setResult] = useState<InstantResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const run = useCallback((q: string) => {
    if (!q.trim()) return;
    setLoading(true);
    client
      .queryMetrics({ query: q })
      .then((r) => {
        setResult(JSON.parse(r.resultJson) as InstantResult);
        setError(null);
      })
      .catch((e: unknown) => {
        setError(String(e));
        setResult(null);
      })
      .finally(() => setLoading(false));
  }, []);

  const rows = result?.result ?? [];

  return (
    <div className="flex flex-col gap-3 min-h-0">
      <div className="flex flex-wrap gap-1.5">
        {PRESETS.map((p) => (
          <button
            key={p.label}
            type="button"
            onClick={() => {
              setQuery(p.query);
              run(p.query);
            }}
            className="border border-line px-2 py-0.5 text-2xs text-dim hover:text-base-content"
          >
            {p.label}
          </button>
        ))}
      </div>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          run(query);
        }}
        className="flex gap-2 items-start"
      >
        <textarea
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            // Enter runs, shift+Enter newlines — a PromQL box is a
            // one-liner far more often than not.
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              run(query);
            }
          }}
          rows={2}
          spellCheck={false}
          aria-label="PromQL query"
          placeholder="PromQL"
          className="flex-1 min-w-0 border border-line bg-base-200 px-2 py-1.5 text-xs font-mono text-base-content outline-none focus:border-primary/60 resize-y"
        />
        <button
          type="submit"
          disabled={loading}
          className="flex-none border border-line px-3 py-1.5 text-xs text-dim hover:text-base-content disabled:opacity-50"
        >
          {loading ? "…" : "run"}
        </button>
      </form>

      <InlineError message={error} onRetry={() => run(query)} onDismiss={() => setError(null)} />

      {result && rows.length === 0 && !error && (
        <p className="text-xs text-dim2">
          No series matched. An empty result is a real answer — the metric may
          exist with no samples in the last scrape.
        </p>
      )}

      {rows.length > 0 && (
        <div className="overflow-x-auto border border-line">
          <table className="w-full text-xs">
            <thead>
              <tr className="text-dim2 text-left border-b border-line">
                <th className="px-2 py-1 font-medium">metric</th>
                <th className="px-2 py-1 font-medium">labels</th>
                <th className="px-2 py-1 font-medium text-right">value</th>
              </tr>
            </thead>
            <tbody className="font-mono">
              {rows.map((r, i) => (
                <tr key={`${JSON.stringify(r.metric)}-${i}`} className="border-b border-line last:border-0">
                  <td className="px-2 py-1 text-base-content whitespace-nowrap">
                    {r.metric.__name__ ?? "—"}
                  </td>
                  <td className="px-2 py-1 text-dim break-all">{labelsOf(r.metric)}</td>
                  <td className="px-2 py-1 text-base-content text-right whitespace-nowrap">
                    {r.value ? formatValue(r.value[1]) : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// The fleet's own observability surface. Deliberately thin next to Grafana,
// which stays the place for time-series work: what this adds is the live
// cell view, where a pod maps back to the session it's running so a
// misbehaving cell is one click from its transcript.
export function Observability({ onSelectTask }: { onSelectTask: (id: string) => void }) {
  const [tab, setTab] = useState<Tab>("topology");

  return (
    <div className="flex-1 min-h-0 overflow-y-auto p-3 sm:p-4 flex flex-col gap-3">
      <div className="flex items-center gap-3 flex-wrap">
        <Segmented value={tab} options={TABS} onChange={setTab} size="sm" />
        <a
          href="https://grafana.bnei.dev/d/agent-fleet-overview"
          target="_blank"
          rel="noreferrer"
          className="text-2xs text-dim2 hover:text-base-content underline underline-offset-2 ml-auto"
        >
          grafana ↗
        </a>
      </div>

      {tab === "topology" ? <FleetTopology onSelectTask={onSelectTask} /> : <MetricsExplorer />}
    </div>
  );
}
