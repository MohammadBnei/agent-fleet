import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import {
  asDisplayMarkdown,
  parseSdkResultSummary,
  parseSdkSignal,
  parseSdkSystemInfo,
  parseSdkToolResult,
  parseSdkToolUse,
  type SdkSignal,
} from "../transcript";
import { Markdown } from "./Markdown";
import { JsonView } from "./JsonView";
import { Collapse } from "./Collapse";

// The single renderer for a transcript entry that isn't a card (question,
// permission, tool-call pair) — those stay with the feed loops that own
// their state. Desktop and mobile previously kept two copies of this switch
// and had already drifted apart; `compact` is the only real difference
// between them (font scale and no max-width on a phone).
//
// Everything the worker relays reaches here, including the out-of-band SDK
// signals that used to be dropped before they left the pod: an expired
// token, a rate limit, a compaction, a hook's output, a long tool call's
// elapsed time. Those are the "why has this gone quiet" answers, so they
// render as real rows rather than another grey log stub.

function LogLine({
  badge,
  badgeClass = "badge-ghost",
  children,
  compact,
}: {
  badge: string;
  badgeClass?: string;
  children: React.ReactNode;
  compact?: boolean;
}) {
  return (
    <div className={`${compact ? "text-[10px]" : "text-[10.5px]"} text-base-content/40 flex items-center gap-1.5`}>
      <span className={`badge badge-xs ${badgeClass}`}>{badge}</span>
      {children}
    </div>
  );
}

// A signal that means the session is in trouble gets an alert row, not a
// log line — silence with no visible cause is the exact failure this whole
// relay exists to remove.
function AlertRow({ tone, title, detail }: { tone: "error" | "warning"; title: string; detail?: string }) {
  return (
    <div className={`rounded-md border px-2.5 py-2 ${tone === "error" ? "border-error/40 bg-error/5" : "border-warning/40 bg-warning/5"}`}>
      <div className={`text-[11px] font-medium ${tone === "error" ? "text-error" : "text-warning"}`}>{title}</div>
      {detail && <div className="text-[10.5px] text-base-content/60 mt-0.5 break-words">{detail}</div>}
    </div>
  );
}

function formatDuration(ms?: number): string | null {
  if (typeof ms !== "number" || ms <= 0) return null;
  return ms < 1000 ? `${ms}ms` : ms < 60_000 ? `${(ms / 1000).toFixed(1)}s` : `${Math.floor(ms / 60_000)}m${Math.round((ms % 60_000) / 1000)}s`;
}

function formatTokens(n?: number): string | null {
  if (typeof n !== "number" || n <= 0) return null;
  return n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n);
}

function SignalView({ signal, compact }: { signal: SdkSignal; compact?: boolean }) {
  switch (signal.sdk) {
    case "auth_status":
      // The single highest-value entry in this file: without it an expired
      // OAuth token is indistinguishable from a hung worker.
      return signal.error ? (
        <AlertRow tone="error" title="Authentication failed" detail={signal.error} />
      ) : (
        <LogLine badge="auth" compact={compact}>
          {signal.isAuthenticating ? "authenticating…" : "authenticated"}
        </LogLine>
      );

    case "assistant_error":
      return <AlertRow tone="error" title={`Model error — ${signal.error ?? "unknown"}`} detail="The turn returned no content." />;

    case "status":
      return signal.status ? (
        <LogLine badge={signal.status} badgeClass="badge-info" compact={compact}>
          <span className="loading loading-dots loading-xs" />
        </LogLine>
      ) : null;

    case "compact_boundary": {
      const pre = formatTokens(signal.compact_metadata?.pre_tokens);
      return (
        <div className="flex items-center gap-2 py-1">
          <div className="h-px flex-1 bg-warning/30" />
          <span className="text-[9.5px] tracking-[0.09em] text-warning/80 whitespace-nowrap">
            CONTEXT COMPACTED{signal.compact_metadata?.trigger ? ` · ${signal.compact_metadata.trigger}` : ""}
            {pre ? ` · ${pre} tokens before` : ""}
          </span>
          <div className="h-px flex-1 bg-warning/30" />
        </div>
      );
    }

    case "hook_response": {
      const failed = typeof signal.exit_code === "number" && signal.exit_code !== 0;
      const label = `${signal.hook_name ?? "hook"}${signal.hook_event ? ` · ${signal.hook_event}` : ""}`;
      const body = [signal.stdout, signal.stderr].filter((s) => s && s.trim()).join("\n");
      if (!body) {
        return (
          <LogLine badge="hook" badgeClass={failed ? "badge-error" : "badge-ghost"} compact={compact}>
            {label}
            {failed ? ` — exit ${signal.exit_code}` : ""}
          </LogLine>
        );
      }
      return (
        <Collapse
          summaryClassName="px-2 py-1"
          summary={
            <span className="text-[10.5px] text-base-content/60">
              <span className={`badge badge-xs mr-1.5 ${failed ? "badge-error" : "badge-ghost"}`}>hook</span>
              {label}
              {failed ? ` — exit ${signal.exit_code}` : ""}
            </span>
          }
        >
          <pre className="text-[10.5px] whitespace-pre-wrap font-mono text-base-content/70">{body}</pre>
        </Collapse>
      );
    }

    case "tool_progress": {
      // Deliberately not paired to its tool call: the SDK's tool_use_id
      // here is synthetic and unique per emission ("bash-progress-30"),
      // not the originating toolu_ id, so there is nothing to join on.
      const elapsed = signal.elapsed_time_seconds;
      return (
        <LogLine badge={signal.tool_name ?? "tool"} badgeClass="badge-info" compact={compact}>
          <span className="loading loading-spinner loading-xs" />
          still running{typeof elapsed === "number" ? ` · ${elapsed}s` : ""}
        </LogLine>
      );
    }

    default:
      // A subtype the SDK added after this was written still relays (the
      // worker passes unknown ones through unmapped), so show it rather
      // than swallowing it.
      return (
        <div className="rounded-md border border-base-content/10 bg-base-200/40 px-2.5 py-2">
          <div className="text-[10px] text-base-content/40 mb-1">{signal.sdk}</div>
          <JsonView value={signal} />
        </div>
      );
  }
}

function ResultView({ entry, compact }: { entry: TranscriptEntry; compact?: boolean }) {
  const r = parseSdkResultSummary(entry.text);
  if (!r) return <div className={`${compact ? "text-[10px]" : "text-[10.5px]"} text-base-content/40`}>{entry.text}</div>;

  const wall = formatDuration(r.durationMs);
  const api = formatDuration(r.durationApiMs);
  const inTok = formatTokens((r.usage?.input_tokens ?? 0) + (r.usage?.cache_read_input_tokens ?? 0) + (r.usage?.cache_creation_input_tokens ?? 0));
  const outTok = formatTokens(r.usage?.output_tokens);
  const parts = [
    `${r.numTurns ?? "?"} turns`,
    `$${(r.totalCostUsd ?? 0).toFixed(3)}`,
    wall && api ? `${wall} (${api} api)` : wall,
    inTok && `${inTok} in`,
    outTok && `${outTok} out`,
  ].filter(Boolean);

  const models = Object.entries(r.modelUsage ?? {});
  const failed = r.isError || (r.subtype && r.subtype !== "success");

  return (
    <div className="flex flex-col gap-1">
      <div className={`${compact ? "text-[10px]" : "text-[10.5px]"} ${failed ? "text-error/80" : "text-base-content/40"} flex items-center gap-1.5 flex-wrap`}>
        {failed && <span className="badge badge-error badge-xs">{r.subtype ?? "error"}</span>}
        {parts.join(" · ")}
      </div>
      {r.errors && r.errors.length > 0 && <AlertRow tone="error" title="Session errors" detail={r.errors.join("; ")} />}
      {r.permissionDenials && r.permissionDenials.length > 0 && (
        <div className="text-[10px] text-warning/70">
          {r.permissionDenials.length} permission {r.permissionDenials.length === 1 ? "denial" : "denials"}
          {": "}
          {r.permissionDenials.map((d) => d.tool_name).filter(Boolean).join(", ")}
        </div>
      )}
      {models.length > 1 && (
        <div className="text-[10px] text-base-content/35">
          {models.map(([name, u]) => `${name.replace(/-\d{8}$/, "")} $${(u.costUSD ?? 0).toFixed(3)}`).join(" · ")}
        </div>
      )}
    </div>
  );
}

function SessionInitView({ entry, compact }: { entry: TranscriptEntry; compact?: boolean }) {
  const info = parseSdkSystemInfo(entry.text);
  if (!info) return null;
  const offline = (info.mcpServers ?? []).filter((s) => s.status !== "connected");
  const rows: [string, string | undefined][] = [
    ["model", info.model],
    ["mode", info.permissionMode],
    ["cwd", info.cwd],
    ["cli", info.claudeCodeVersion],
    ["output style", info.outputStyle],
    ["tools", info.tools?.length ? String(info.tools.length) : undefined],
    ["skills", info.skills?.length ? info.skills.join(", ") : undefined],
    ["plugins", info.plugins?.length ? info.plugins.map((p) => p.name).join(", ") : undefined],
    ["agents", info.agents?.length ? info.agents.join(", ") : undefined],
    ["mcp", (info.mcpServers ?? []).map((s) => `${s.name} (${s.status})`).join(", ") || undefined],
  ];

  return (
    <Collapse
      summaryClassName="px-2 py-1"
      summary={
        <span className={`${compact ? "text-[10px]" : "text-[10.5px]"} text-base-content/40`}>
          <span className="badge badge-ghost badge-xs mr-1.5">session</span>
          {info.model ? `started · ${info.model}` : "session started"}
          {/* An MCP server that failed to connect silently removes tools
              the agent is about to be asked to use. */}
          {offline.length > 0 && <span className="text-error ml-1.5">· {offline.length} mcp offline</span>}
        </span>
      }
    >
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-[10.5px]">
        {rows
          .filter(([, value]) => value)
          .map(([label, value]) => (
            <div key={label} className="contents">
              <dt className="text-base-content/40">{label}</dt>
              <dd className="text-base-content/70 break-all">{value}</dd>
            </div>
          ))}
      </dl>
    </Collapse>
  );
}

export function TranscriptEntryView({ entry, compact }: { entry: TranscriptEntry; compact?: boolean }) {
  const logSize = compact ? "text-[10px]" : "text-[10.5px]";

  if (entry.type === TranscriptEntryType.SYSTEM) {
    const signal = parseSdkSignal(entry.text);
    return signal ? <SignalView signal={signal} compact={compact} /> : <SessionInitView entry={entry} compact={compact} />;
  }

  if (entry.type === TranscriptEntryType.ASSISTANT) {
    // Only reached for a thinking block — a real tool_use is rendered by
    // the feed's ToolCallItem pair instead.
    const info = parseSdkToolUse(entry.text);
    if (info?.kind !== "thinking" || !info.text) return null;
    return (
      <Collapse summaryClassName="px-2 py-1" summary={<span className={`${logSize} text-base-content/35 italic`}>thinking</span>}>
        <div className="text-[11px] leading-relaxed text-base-content/50 italic whitespace-pre-wrap">{info.text}</div>
      </Collapse>
    );
  }

  // An orphaned tool_result — no matching call, e.g. truncated history.
  // Desktop used to drop this through the prose bubble and render raw JSON
  // as markdown; mobile already had this branch.
  if (entry.type === TranscriptEntryType.USER) {
    const tr = parseSdkToolResult(entry.text);
    return (
      <div className={`rounded-md border px-2.5 py-2 ${tr?.isError ? "border-error/40 bg-error/5" : "border-base-content/10 bg-base-200/40"}`}>
        {typeof tr?.content === "string" ? (
          <pre className="text-[10.5px] whitespace-pre-wrap font-mono text-base-content/70">{tr.content}</pre>
        ) : (
          <JsonView value={tr?.content ?? entry.text} />
        )}
      </div>
    );
  }

  if (entry.type === TranscriptEntryType.RESULT) return <ResultView entry={entry} compact={compact} />;

  // Backend-verified events (a real transcript row core only writes after a
  // genuine dashboard/Discord action) get a compact log-line treatment
  // distinct from the generic prose bubble below — otherwise these are
  // indistinguishable from the model's own narration of the same claim
  // (e.g. "good, user approved" in its own text), which is real but
  // unverified.
  if (entry.type === TranscriptEntryType.APPROVE) {
    return (
      <LogLine badge="approved" badgeClass="badge-success" compact={compact}>
        plan approved — implementing
      </LogLine>
    );
  }
  if (entry.type === TranscriptEntryType.ABORT) {
    return (
      <LogLine badge="killed" badgeClass="badge-warning" compact={compact}>
        {entry.text || "task killed"}
      </LogLine>
    );
  }
  if (entry.type === TranscriptEntryType.INTERRUPT) {
    return (
      <LogLine badge="interrupted" badgeClass="badge-info" compact={compact}>
        {entry.text || "current turn interrupted"}
      </LogLine>
    );
  }
  if (entry.type === TranscriptEntryType.PERMISSION_MODE) {
    return (
      <LogLine badge="mode" badgeClass="badge-info" compact={compact}>
        permission mode → {entry.text}
      </LogLine>
    );
  }

  return (
    <div className={compact ? undefined : "max-w-[760px]"}>
      <div
        className={`${compact ? "text-[12.5px]" : "text-[12px]"} leading-relaxed ${entry.from === "human" ? "text-primary" : "text-base-content/90"}`}
      >
        <Markdown text={asDisplayMarkdown(entry)} />
      </div>
    </div>
  );
}
