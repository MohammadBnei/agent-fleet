import { useEffect, useState } from "react";
import { client } from "../connectClient";
import type { CellNode, TopologyEdge } from "../gen/agentfleet/v1/dashboard_pb";
import { pollVisible } from "../pollVisible";
import { InlineError } from "./InlineError";

// The fleet drawn as cells: two long-lived hubs and one cell per live pod.
//
// ponytail: hand-laid-out inline SVG, no d3/cytoscape. The graph is three
// fixed tiers and at most MAX_IN_FLIGHT_TASKS worker cells — a bounded,
// known shape, in a bundle that gets compiled into core's binary. A layout
// engine earns its weight when positions can't be computed in a loop; here
// they can. Reach for one if the fleet ever grows a topology that isn't
// tiers.

const VIEW_W = 800;
const TIER_Y = { core: 60, provisioner: 190, worker: 330 };
const HUB_W = 220;
const HUB_H = 56;
const CELL_W = 150;
const CELL_H = 64;

// Matches the four statuses core's cellStatus() emits. Deliberately theme
// tokens, not hex: this renders in both themes.
const STATUS_CLS: Record<string, { stroke: string; text: string; dot: string }> = {
  healthy: { stroke: "stroke-success", text: "text-success", dot: "fill-success" },
  degraded: { stroke: "stroke-warning", text: "text-warning", dot: "fill-warning" },
  failing: { stroke: "stroke-error", text: "text-error", dot: "fill-error" },
  idle: { stroke: "stroke-line3", text: "text-dim2", dot: "fill-dim2" },
};

function statusOf(status: string) {
  return STATUS_CLS[status] ?? STATUS_CLS.idle;
}

type Placed = CellNode & { x: number; y: number; w: number; h: number };

// Positions every cell. Workers spread evenly across the full width, so a
// fleet of one centres it rather than pinning it to the left edge.
export function layout(nodes: CellNode[]): Placed[] {
  const workers = nodes.filter((n) => n.type === "worker" || n.type === "e2e");
  const step = VIEW_W / (workers.length + 1);
  let i = 0;

  return nodes.map((n) => {
    if (n.type === "core" || n.type === "provisioner") {
      return {
        ...n,
        x: VIEW_W / 2 - HUB_W / 2,
        y: TIER_Y[n.type as "core" | "provisioner"],
        w: HUB_W,
        h: HUB_H,
      };
    }
    i += 1;
    return { ...n, x: step * i - CELL_W / 2, y: TIER_Y.worker, w: CELL_W, h: CELL_H };
  });
}

const centre = (p: Placed) => ({ x: p.x + p.w / 2, y: p.y + p.h / 2 });

function truncate(s: string, n: number): string {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s;
}

function formatRate(rps: number): string {
  if (!rps) return "";
  return rps < 1 ? `${rps.toFixed(2)}/s` : `${rps.toFixed(1)}/s`;
}

export function FleetTopology({ onSelectTask }: { onSelectTask: (id: string) => void }) {
  const [nodes, setNodes] = useState<CellNode[]>([]);
  const [edges, setEdges] = useState<TopologyEdge[]>([]);
  const [metricsError, setMetricsError] = useState("");
  const [error, setError] = useState<string | null>(null);

  useEffect(
    () =>
      pollVisible(() => {
        client
          .getFleetTopology({})
          .then((r) => {
            setNodes(r.nodes);
            setEdges(r.edges);
            setMetricsError(r.metricsError);
            setError(null);
          })
          .catch((e: unknown) => setError(String(e)));
      }, 5000),
    [],
  );

  const placed = layout(nodes);
  const byId = new Map(placed.map((p) => [p.id, p]));
  const workers = placed.filter((p) => p.type === "worker" || p.type === "e2e");
  // Grow the canvas with the tiers rather than letting the bottom row clip.
  const viewH = TIER_Y.worker + CELL_H + 40;

  return (
    <div className="flex flex-col gap-3 min-h-0">
      {error && <InlineError message={error} onDismiss={() => setError(null)} />}

      {/* Prometheus being down is not the topology being wrong — the cells
          are still true, they just have no rates. Saying so beats rendering
          a busy fleet with silent zeroes. */}
      {metricsError && (
        <p className="text-xs text-warning border border-line px-2 py-1">
          rates unavailable — {metricsError}
        </p>
      )}

      {/* overflow-x-auto, per the page-never-scrolls-sideways rule: the SVG
          scales to the container, but a wide fleet keeps its own scroller. */}
      <div className="overflow-x-auto border border-line bg-base-200">
        <svg
          viewBox={`0 0 ${VIEW_W} ${viewH}`}
          className="w-full h-auto min-w-[560px]"
          role="img"
          aria-label={`Fleet topology: ${workers.length} live worker cells`}
        >
          {edges.map((e, i) => {
            const from = byId.get(e.from);
            const to = byId.get(e.to);
            if (!from || !to) return null;
            const a = centre(from);
            const b = centre(to);
            // Dashed for the report path (worker → core), solid for the
            // spawn path (core → provisioner → worker). They're genuinely
            // different relationships — see docs/adr/0020's hub-and-spoke
            // rule — and drawing them identically hides that.
            const isReport = to.type === "core" && from.type !== "core";
            return (
              <g key={`${e.from}->${e.to}-${i}`}>
                <line
                  x1={a.x}
                  y1={a.y}
                  x2={b.x}
                  y2={b.y}
                  className={isReport ? "stroke-line3" : "stroke-line2"}
                  strokeWidth={1}
                  strokeDasharray={isReport ? "3 4" : undefined}
                />
                {e.rate > 0 && (
                  <text
                    x={(a.x + b.x) / 2 + 6}
                    y={(a.y + b.y) / 2 - 4}
                    className="fill-dim2 text-[9px]"
                  >
                    {formatRate(e.rate)}
                  </text>
                )}
              </g>
            );
          })}

          {placed.map((p) => {
            const s = statusOf(p.status);
            const isHub = p.type === "core" || p.type === "provisioner";
            const clickable = Boolean(p.taskId);
            return (
              <g
                key={p.id}
                transform={`translate(${p.x} ${p.y})`}
                className={clickable ? "cursor-pointer" : undefined}
                onClick={clickable ? () => onSelectTask(p.taskId) : undefined}
                role={clickable ? "button" : undefined}
                tabIndex={clickable ? 0 : undefined}
                onKeyDown={
                  clickable
                    ? (ev) => {
                        if (ev.key === "Enter" || ev.key === " ") onSelectTask(p.taskId);
                      }
                    : undefined
                }
              >
                <rect
                  width={p.w}
                  height={p.h}
                  className={`fill-base-100 ${isHub ? "stroke-primary" : s.stroke}`}
                  strokeWidth={1}
                />
                <circle cx={10} cy={11} r={3} className={isHub ? "fill-primary" : s.dot} />
                <text x={20} y={14} className="fill-base-content text-[10px] font-medium">
                  {isHub ? p.type : truncate(p.repo || "worker", 18)}
                </text>
                <text x={8} y={30} className="fill-dim2 text-[9px]">
                  {truncate(p.label, isHub ? 34 : 22)}
                </text>
                {isHub ? (
                  <text x={8} y={46} className="fill-dim text-[9px]">
                    {p.type === "core"
                      ? `${p.metrics.live_pods ?? 0}/${p.metrics.max_in_flight ?? 0} pods${
                          p.metrics.rps ? ` · ${formatRate(p.metrics.rps)}` : ""
                        }`
                      : formatRate(p.metrics.rps ?? 0) || "idle"}
                  </text>
                ) : (
                  <>
                    <text x={8} y={46} className="fill-dim text-[9px]">
                      #{p.taskId.slice(0, 6)}
                    </text>
                    <text x={8} y={58} className={`text-[9px] ${s.text} fill-current`}>
                      {p.status}
                    </text>
                  </>
                )}
              </g>
            );
          })}
        </svg>
      </div>

      {workers.length === 0 && (
        <p className="text-xs text-dim2">
          No live worker pods. Cells appear here as sessions get provisioned.
        </p>
      )}
    </div>
  );
}
