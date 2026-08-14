// Hand-written Prometheus text format exporter for SDK metrics.
// No Prometheus client lib exists for Bun yet.

interface MetricValue {
  value: number;
  labels: Record<string, string>;
}

class Counter {
  private values = new Map<string, number>();

  inc(labels: Record<string, string>, amount = 1): void {
    const key = this.labelKey(labels);
    this.values.set(key, (this.values.get(key) ?? 0) + amount);
  }

  render(name: string, help: string, labelNames: string[]): string {
    const lines: string[] = [];
    lines.push(`# HELP ${name} ${help}`);
    lines.push(`# TYPE ${name} counter`);
    for (const [key, value] of this.values) {
      lines.push(`${name}{${key}} ${value}`);
    }
    return lines.join("\n");
  }

  private labelKey(labels: Record<string, string>): string {
    return Object.entries(labels)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => `${k}="${v}"`)
      .join(",");
  }
}

class Gauge {
  private values = new Map<string, number>();

  set(labels: Record<string, string>, value: number): void {
    const key = this.labelKey(labels);
    this.values.set(key, value);
  }

  render(name: string, help: string, labelNames: string[]): string {
    const lines: string[] = [];
    lines.push(`# HELP ${name} ${help}`);
    lines.push(`# TYPE ${name} gauge`);
    for (const [key, value] of this.values) {
      lines.push(`${name}{${key}} ${value}`);
    }
    return lines.join("\n");
  }

  private labelKey(labels: Record<string, string>): string {
    return Object.entries(labels)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => `${k}="${v}"`)
      .join(",");
  }
}

const turnsTotal = new Counter();
const costUsdTotal = new Counter();
const tokensTotal = new Counter();
const compactionsTotal = new Counter();
const preCompactTokens = new Gauge();
const permissionDenialsTotal = new Counter();

const taskId = process.env.TASK_ID || "unknown";

export function recordSdkMessage(msg: { type: string; [key: string]: unknown }): void {
  if (msg.type === "result") {
    const model = (msg.model as string) ?? "unknown";
    const numTurns = (msg.num_turns as number) ?? 0;
    const totalCostUsd = (msg.total_cost_usd as number) ?? 0;
    const usage = msg.usage as
      | { input_tokens?: number; output_tokens?: number; cache_read_input_tokens?: number }
      | undefined;

    if (numTurns > 0) {
      turnsTotal.inc({ task_id: taskId, model }, numTurns);
    }
    if (totalCostUsd > 0) {
      costUsdTotal.inc({ task_id: taskId, model }, totalCostUsd);
    }
    if (usage?.input_tokens) {
      tokensTotal.inc({ task_id: taskId, model, type: "input" }, usage.input_tokens);
    }
    if (usage?.output_tokens) {
      tokensTotal.inc({ task_id: taskId, model, type: "output" }, usage.output_tokens);
    }
    if (usage?.cache_read_input_tokens) {
      tokensTotal.inc({ task_id: taskId, model, type: "cache_read" }, usage.cache_read_input_tokens);
    }

    const denials = msg.permission_denials as { tool_name?: string }[] | undefined;
    if (denials && denials.length > 0) {
      for (const denial of denials) {
        const toolName = denial.tool_name ?? "unknown";
        permissionDenialsTotal.inc({ task_id: taskId, tool_name: toolName });
      }
    }
  }

  if (msg.type === "system" && msg.subtype === "compact_boundary") {
    compactionsTotal.inc({ task_id: taskId });
    const preTokens = (msg.compact_metadata as { pre_tokens?: number } | undefined)?.pre_tokens;
    if (preTokens !== undefined) {
      preCompactTokens.set({ task_id: taskId }, preTokens);
    }
  }
}

export function renderMetrics(): string {
  const parts: string[] = [];
  parts.push(turnsTotal.render("agentfleet_sdk_turns_total", "SDK turns by task and model", ["task_id", "model"]));
  parts.push(costUsdTotal.render("agentfleet_sdk_cost_usd_total", "SDK cost in USD by task and model", ["task_id", "model"]));
  parts.push(tokensTotal.render("agentfleet_sdk_tokens_total", "SDK tokens by task, model, type", ["task_id", "model", "type"]));
  parts.push(compactionsTotal.render("agentfleet_sdk_compactions_total", "SDK compactions by task", ["task_id"]));
  parts.push(preCompactTokens.render("agentfleet_sdk_pre_compact_tokens", "Last pre-compaction token count", ["task_id"]));
  parts.push(permissionDenialsTotal.render("agentfleet_sdk_permission_denials_total", "Permission denials by task and tool", ["task_id", "tool_name"]));
  return parts.filter(Boolean).join("\n\n") + "\n";
}
