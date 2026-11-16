// Timings of what this worker does, sent to core as spans (docs/adr/0081 §7).
//
// The worker holds no OpenTelemetry and cannot reach a collector: it records
// names, local ids and times, posts them to its own pod's sidecar (POST
// /spans), and core, which knows the session and owns the exporter, turns
// them into real spans. So this file is a recorder, not a tracer. Names, ids
// and numbers only: never a prompt, tool input or tool output.
//
// ponytail: hand-rolled on purpose (no OTel JS SDK in the worker image). Move
// to the OTel JS SDK if this outgrows ~80 lines of logic.
//
// Shape: one `invoke_agent` span per turn (first SDK message after the last
// result, to the next result), and one `execute_tool <name>` span per tool_use
// block, closed by its tool_result. A tool the SDK reports under a subagent
// (parent_tool_use_id) nests under that Task call. A turn's spans go out
// together when its result arrives, so core can link parents within a batch.
// Token and cost figures are DELTAS: the SDK's total_cost_usd and modelUsage
// are cumulative across the turns of one streaming-input process, and a pod
// that resumes a session starts its process at zero.
//
// Before the first turn, one `worker.startup` span covers the pod's start:
// process start to the session id being known, with a child per step
// (worker.boot, worker.get_session, worker.setup, worker.query_start,
// worker.first_sdk_message, worker.session_id, worker.save_session_id). In
// streaming-input mode the CLI says nothing until the first input is queued,
// so first_sdk_message includes that message's delivery. It has no result uuid
// to key it, so its key is `startup-<pod name>`: one trace per warm (a pod is
// one warm: RestartPolicy Never, BackoffLimit 0), and found in Tempo by
// `{ name = "worker.startup" && span.fleet.session.id = "<id>" }`.
import { log } from "./log.js";

export type WireSpan = {
  name: string;
  id: string;
  parent?: string;
  turn: string;
  startNs: number;
  endNs: number;
  status: "ok" | "error" | "";
  attrs: Record<string, string | number | boolean>;
};
type Pending = Omit<WireSpan, "turn">;
export type PostSpans = (spans: WireSpan[]) => Promise<unknown>;

export type TurnResult = {
  uuid?: string;
  isError?: boolean;
  subtype?: string;
  numTurns?: number;
  /** Cumulative for this process. */
  totalCostUsd?: number;
  /** Cumulative for this process (sum of modelUsage). */
  tokens?: {
    input: number;
    output: number;
    cacheRead: number;
    cacheCreation: number;
  };
  models?: string[];
};

const nowNs = () => Date.now() * 1e6;
// Counter-style local ids are fine: core never trusts them, it only matches
// parents within one batch and mints its own ids.
let nextLocal = 0;
const localId = () => `w${++nextLocal}`;

const delta = (total: number, prev: number) => Math.max(0, total - prev);

export class SpanRecorder {
  private turn: { id: string; startNs: number } | null = null;
  private done: Pending[] = [];
  private open = new Map<
    string,
    { id: string; parent: string; name: string; startNs: number }
  >();
  private prev = {
    cost: 0,
    input: 0,
    output: 0,
    cacheRead: 0,
    cacheCreation: 0,
  };
  private inflight = new Set<Promise<unknown>>();
  private boot: {
    id: string;
    key: string;
    startNs: number;
    spans: Pending[];
  } | null = null;

  constructor(
    private post: PostSpans,
    private clock: () => number = nowNs,
  ) {}

  /** Opens the startup trace at process start (see the file comment). */
  startupBegin(startNs: number, key: string): void {
    this.boot = { id: localId(), key, startNs, spans: [] };
  }

  /** One step of startup, ending now unless endNs is given. */
  startupStep(
    name: string,
    startNs: number,
    endNs = this.clock(),
    attrs: WireSpan["attrs"] = {},
    status: WireSpan["status"] = "ok",
  ): void {
    this.boot?.spans.push({
      name,
      id: localId(),
      parent: this.boot.id,
      startNs,
      endNs,
      status,
      attrs,
    });
  }

  /** Closes startup and sends its spans, root first. "" = cut short. */
  startupEnd(status: WireSpan["status"] = "ok"): void {
    const b = this.boot;
    if (!b) return;
    this.boot = null;
    const root: Pending = {
      name: "worker.startup",
      id: b.id,
      startNs: b.startNs,
      endNs: this.clock(),
      status,
      attrs: status === "" ? { unfinished: true } : {},
    };
    this.send([root, ...b.spans].map((s) => ({ ...s, turn: b.key })));
  }

  /** Starts the turn at the first SDK message after the previous result. */
  begin(): void {
    if (!this.turn) this.turn = { id: localId(), startNs: this.clock() };
  }

  toolStart(
    toolUseId: unknown,
    name: unknown,
    parentToolUseId?: unknown,
  ): void {
    if (typeof toolUseId !== "string" || typeof name !== "string") return;
    this.begin();
    if (this.open.has(toolUseId)) return;
    const sub =
      typeof parentToolUseId === "string" && this.open.get(parentToolUseId);
    this.open.set(toolUseId, {
      id: localId(),
      parent: sub ? sub.id : (this.turn as { id: string }).id,
      name,
      startNs: this.clock(),
    });
  }

  toolEnd(toolUseId: unknown, isError: boolean): void {
    if (typeof toolUseId !== "string") return;
    this.closeTool(toolUseId, isError ? "error" : "ok");
  }

  private closeTool(toolUseId: string, status: WireSpan["status"]): void {
    const t = this.open.get(toolUseId);
    if (!t) return;
    this.open.delete(toolUseId);
    const tool = t.name;
    this.done.push({
      name: `execute_tool ${tool}`,
      id: t.id,
      parent: t.parent,
      startNs: t.startNs,
      endNs: this.clock(),
      status,
      attrs: {
        "gen_ai.operation.name": "execute_tool",
        "gen_ai.tool.name": tool,
        "gen_ai.tool.call.id": toolUseId,
        ...(status === "error" ? { "error.type": "tool_error" } : {}),
        ...(status === "" ? { unfinished: true } : {}),
      },
    });
  }

  /** Closes the turn at its result message and sends its spans. */
  endTurn(r: TurnResult): void {
    this.begin();
    const attrs: WireSpan["attrs"] = {
      "gen_ai.operation.name": "invoke_agent",
    };
    if (r.subtype) attrs["result.subtype"] = r.subtype;
    if (r.numTurns !== undefined) attrs["result.num_turns"] = r.numTurns;
    if (r.models?.length) attrs["gen_ai.request.model"] = r.models.join(",");
    const t = r.tokens;
    // A crash or startup-error result can carry zeroed totals; it says nothing
    // about spend, so it neither reports usage nor moves the baseline (else
    // the next real result would count everything again).
    const zeroed =
      r.isError &&
      !r.totalCostUsd &&
      (!t || t.input + t.output + t.cacheRead + t.cacheCreation === 0);
    if (!zeroed) {
      // A reset (a new process starts every total at zero) is read once, from
      // the cost total, for all the fields.
      if (typeof r.totalCostUsd === "number" && r.totalCostUsd < this.prev.cost)
        this.prev = {
          cost: 0,
          input: 0,
          output: 0,
          cacheRead: 0,
          cacheCreation: 0,
        };
      if (typeof r.totalCostUsd === "number") {
        attrs["gen_ai.usage.cost_micro_usd"] = Math.round(
          delta(r.totalCostUsd, this.prev.cost) * 1e6,
        );
        this.prev.cost = r.totalCostUsd;
      }
      if (t) {
        attrs["gen_ai.usage.input_tokens"] = delta(t.input, this.prev.input);
        attrs["gen_ai.usage.output_tokens"] = delta(t.output, this.prev.output);
        attrs["gen_ai.usage.cache_read.input_tokens"] = delta(
          t.cacheRead,
          this.prev.cacheRead,
        );
        attrs["gen_ai.usage.cache_creation.input_tokens"] = delta(
          t.cacheCreation,
          this.prev.cacheCreation,
        );
        this.prev = {
          ...this.prev,
          input: t.input,
          output: t.output,
          cacheRead: t.cacheRead,
          cacheCreation: t.cacheCreation,
        };
      }
    }
    this.finish(r.uuid, r.isError ? "error" : "ok", attrs);
  }

  private finish(
    key: string | undefined,
    status: WireSpan["status"],
    attrs: WireSpan["attrs"],
  ): void {
    const turn = this.turn;
    if (!turn) return;
    for (const id of [...this.open.keys()]) this.closeTool(id, "");
    // The turn span goes FIRST: core and the sidecar cut a long batch from the
    // end, and the root is the one span the rest hang from.
    const root: Pending = {
      name: "invoke_agent",
      id: turn.id,
      startNs: turn.startNs,
      endNs: this.clock(),
      status,
      attrs,
    };
    // The SDK's result uuid names the turn on every pod; an unfinished turn
    // (the pod is going away) is keyed by when it began.
    const turnKey = key || `partial-${turn.startNs}`;
    const batch = [root, ...this.done].map((s) => ({ ...s, turn: turnKey }));
    this.done = [];
    this.turn = null;
    this.send(batch);
  }

  private send(batch: WireSpan[]): void {
    const p = (async () => this.post(batch))()
      .catch((err) => log("warn", "span export failed", { error: String(err) }))
      .finally(() => this.inflight.delete(p));
    this.inflight.add(p);
  }

  /** Sends a turn still in progress (the pod is ending) and waits for every post. */
  async flush(): Promise<void> {
    this.startupEnd("");
    if (this.turn)
      this.finish(undefined, "", {
        "gen_ai.operation.name": "invoke_agent",
        unfinished: true,
      });
    await Promise.allSettled([...this.inflight]);
  }
}

// The session being recorded, for the SIGTERM handler (index.ts), which has no
// other way to reach it: resident pods end by SIGTERM or the idle timeout in
// the middle of one continuous query().
let active: SpanRecorder | null = null;
export const setActiveRecorder = (r: SpanRecorder | null): void => {
  active = r;
};
export const activeRecorder = (): SpanRecorder | null => active;
export const flushActiveSpans = (): Promise<void> =>
  active ? active.flush() : Promise.resolve();
