import { TranscriptEntryType, type TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import {
  asDisplayMarkdown,
  parseSdkResultSummary,
  parseSdkSignal,
  parseSdkSystemInfo,
  parseSdkToolResult,
  parseSdkToolUse,
  type SdkResultSummary,
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

// docs/dashboard-spec.md §8 item 2: a lifecycle line and the agent's actual
// prose used to be the same ~10px grey text. A rule with the label on the left
// and the summary on the right reads as a boundary marker rather than as
// something someone said.
function LogLine({
  badge,
  badgeClass,
  children,
  compact,
}: {
  badge: string;
  // Retained for the few callers that tint the label (an error subtype, a
  // mode change) — now a plain text colour, not a DaisyUI badge class.
  badgeClass?: string;
  children?: React.ReactNode;
  compact?: boolean;
}) {
  const size = compact ? "text-2xs" : "text-xs";
  const tone = badgeClass?.includes("error")
    ? "text-error"
    : badgeClass?.includes("warning")
      ? "text-warning"
      : badgeClass?.includes("info")
        ? "text-info"
        : "text-dim2";
  return (
    <div className="flex items-center gap-2.5">
      <span className={`${size} ${tone} whitespace-nowrap`}>{badge}</span>
      <span className="flex-1 h-px bg-line3" />
      {children && <span className={`${size} text-dim2 text-right min-w-0`}>{children}</span>}
    </div>
  );
}

// A signal that means the session is in trouble gets an alert row, not a
// log line — silence with no visible cause is the exact failure this whole
// relay exists to remove.
function AlertRow({ tone, title, detail }: { tone: "error" | "warning"; title: string; detail?: string }) {
  return (
    <div
      className={`border px-3 py-2.5 flex gap-2.5 items-start ${
        tone === "error" ? "border-pink-line bg-pink-bg" : "border-orange-line bg-orange-bg"
      }`}
    >
      <span className={`text-sm flex-none ${tone === "error" ? "text-error" : "text-warning"}`}>!</span>
      <div className="min-w-0">
        <div className={`text-sm ${tone === "error" ? "text-error" : "text-warning"}`}>{title}</div>
        {detail && <div className="text-xs text-dim mt-0.5 break-words">{detail}</div>}
      </div>
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
        <LogLine badge={signal.status} badgeClass="info" compact={compact}>
          <span className="loading loading-dots loading-xs" />
        </LogLine>
      ) : null;

    case "compact_boundary": {
      const pre = formatTokens(signal.compact_metadata?.pre_tokens);
      return (
        <div className="flex items-center gap-2 py-1">
          <div className="h-px flex-1 bg-warning/30" />
          <span className="text-2xs tracking-[0.09em] text-warning/80 whitespace-nowrap">
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
          className="border-0 bg-transparent"
          summaryClassName="px-0 py-0 bg-transparent hover:bg-transparent"
          contentClassName="px-0 pt-1.5"
          summary={
            <div className="flex items-center gap-2.5">
              <span className={`text-xs whitespace-nowrap ${failed ? "text-warning" : "text-dim2"}`}>
                ▸ hook {failed ? `failed — exit ${signal.exit_code}` : ""}
              </span>
              <span className="flex-1 h-px bg-line3" />
              <span className="text-xs text-dim2 text-right min-w-0 truncate">{label}</span>
            </div>
          }
        >
          <pre className="text-xs whitespace-pre-wrap font-mono text-dim">{body}</pre>
        </Collapse>
      );
    }

    case "tool_progress": {
      // Deliberately not paired to its tool call: the SDK's tool_use_id
      // here is synthetic and unique per emission ("bash-progress-30"),
      // not the originating toolu_ id, so there is nothing to join on.
      const elapsed = signal.elapsed_time_seconds;
      return (
        <LogLine badge={signal.tool_name ?? "tool"} badgeClass="info" compact={compact}>
          <span className="loading loading-spinner loading-xs" />
          still running{typeof elapsed === "number" ? ` · ${elapsed}s` : ""}
        </LogLine>
      );
    }

    // The four below all shipped as raw JSON dumps because SignalView had no
    // case for them — a `provider: "github" / url: "https://…"` key/value
    // block where one line would do. Each is a real answer to a question
    // someone asks of this feed, which is exactly why the dump was worth
    // replacing rather than silencing.
    case "code_change_published":
      return (
        <LogLine badge="published" badgeClass="info" compact={compact}>
          {signal.url ? (
            <a className="text-primary underline" href={signal.url} target="_blank" rel="noreferrer">
              {signal.identifier ? `#${signal.identifier}` : "link"}
            </a>
          ) : (
            signal.identifier
          )}
          {signal.repo ? ` · ${signal.repo}` : ""}
        </LogLine>
      );

    case "task_notification": {
      // Only reaches here for a NON-subagent task — a backgrounded Bash, whose
      // tool row went "ok" the moment it launched and has said nothing since.
      const failed = signal.status !== undefined && signal.status !== "completed";
      return (
        <LogLine badge="background" badgeClass={failed ? "warning" : undefined} compact={compact}>
          {signal.summary ?? signal.status}
        </LogLine>
      );
    }

    case "vcs_state_changed":
      return (
        <LogLine badge={signal.kind ?? "git"} compact={compact}>
          {signal.branch}
        </LogLine>
      );

    case "api_retry":
      // Not an alarm (SessionFeed excludes it deliberately) — the SDK retries
      // up to max_retries on its own. It is still the answer to "why did this
      // go quiet for a minute", so it must not be silent either.
      return (
        <LogLine badge="api retry" badgeClass="warning" compact={compact}>
          {[
            signal.attempt && `${signal.attempt}/${signal.max_retries ?? "?"}`,
            signal.error_status,
            signal.error,
          ]
            .filter(Boolean)
            .join(" · ")}
        </LogLine>
      );

    case "permission_denied":
      // A denial the fleet never asked a human about — canUseTool's auto-mode
      // rules, or the classifier. Without this it was JSON, and "why did it
      // stop doing that" had no readable answer anywhere.
      return (
        <LogLine badge={`${signal.tool_name ?? "tool"} denied`} badgeClass="warning" compact={compact}>
          {signal.decision_reason ?? signal.status}
        </LogLine>
      );

    default:
      // A subtype the SDK added after this was written still relays (the
      // worker passes unknown ones through unmapped), so show it rather
      // than swallowing it.
      return (
        <div className="border border-line bg-base-300/40 px-2.5 py-2">
          <div className="text-xs text-dim2 mb-1">{signal.sdk}</div>
          <JsonView value={signal} />
        </div>
      );
  }
}

// `result` is the summary with its session-cumulative fields already
// reduced to this turn's share (SessionFeed owns that, since only the walk
// knows which result came before). Falling back to the raw parse keeps the
// component usable on its own — at the cost of the cumulative reading, so
// prefer passing it.
function ResultView({
  entry,
  compact,
  result,
}: {
  entry: TranscriptEntry;
  compact?: boolean;
  result?: SdkResultSummary | null;
}) {
  const r = result ?? parseSdkResultSummary(entry.text);
  if (!r) return <LogLine badge="turn ended" compact={compact}>{entry.text}</LogLine>;

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
      <LogLine
        badge={failed ? `turn failed · ${r.subtype ?? "error"}` : "turn ended"}
        badgeClass={failed ? "error" : undefined}
        compact={compact}
      >
        {parts.join(" · ")}
      </LogLine>
      {r.errors && r.errors.length > 0 && <AlertRow tone="error" title="Session errors" detail={r.errors.join("; ")} />}
      {r.permissionDenials && r.permissionDenials.length > 0 && (
        <div className="text-xs text-warning">
          {r.permissionDenials.length} permission {r.permissionDenials.length === 1 ? "denial" : "denials"}
          {": "}
          {r.permissionDenials.map((d) => d.tool_name).filter(Boolean).join(", ")}
        </div>
      )}
      {models.length > 1 && (
        <div className="text-xs text-dim2">
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
      className="border-0 bg-transparent"
      summaryClassName="px-0 py-0 bg-transparent hover:bg-transparent"
      contentClassName="px-0 pt-1.5"
      summary={
        <div className="flex items-center gap-2.5">
          <span className={`${compact ? "text-2xs" : "text-xs"} text-dim2 whitespace-nowrap`}>
            session started
          </span>
          <span className="flex-1 h-px bg-line3" />
          <span className={`${compact ? "text-2xs" : "text-xs"} text-dim2 text-right min-w-0 truncate`}>
            {[
              info.model,
              info.permissionMode && `${info.permissionMode} mode`,
              info.tools?.length ? `${info.tools.length} tools` : null,
              info.mcpServers?.length ? `${info.mcpServers.length} mcp` : null,
            ]
              .filter(Boolean)
              .join(" · ")}
            {/* An MCP server that failed to connect silently removes tools the
                agent is about to be asked to use. */}
            {offline.length > 0 && <span className="text-error"> · {offline.length} offline</span>}
          </span>
        </div>
      }
    >
      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs">
        {rows
          .filter(([, value]) => value)
          .map(([label, value]) => (
            <div key={label} className="contents">
              <dt className="text-dim2">{label}</dt>
              <dd className="text-dim break-all">{value}</dd>
            </div>
          ))}
      </dl>
    </Collapse>
  );
}

export function TranscriptEntryView({
  entry,
  compact,
  result,
}: {
  entry: TranscriptEntry;
  compact?: boolean;
  result?: SdkResultSummary | null;
}) {
  const logSize = compact ? "text-2xs" : "text-xs";

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
      <Collapse
        className="border-0 bg-transparent"
        summaryClassName="px-0 py-0 bg-transparent hover:bg-transparent"
        contentClassName="px-0 pt-1.5"
        summary={<span className={`${logSize} text-dim2 hover:text-dim`}>▸ thinking</span>}
      >
        <div className="text-xs leading-[1.7] text-dim whitespace-pre-wrap">{info.text}</div>
      </Collapse>
    );
  }

  // An orphaned tool_result — no matching call, e.g. truncated history.
  // Desktop used to drop this through the prose bubble and render raw JSON
  // as markdown; mobile already had this branch.
  if (entry.type === TranscriptEntryType.USER) {
    const tr = parseSdkToolResult(entry.text);
    return (
      <div className={`border px-2.5 py-2 ${tr?.isError ? "border-pink-line bg-pink-bg" : "border-line bg-base-300/40"}`}>
        {typeof tr?.content === "string" ? (
          <pre className="text-xs whitespace-pre-wrap font-mono text-dim">{tr.content}</pre>
        ) : (
          <JsonView value={tr?.content ?? entry.text} />
        )}
      </div>
    );
  }

  if (entry.type === TranscriptEntryType.RESULT)
    return <ResultView entry={entry} compact={compact} result={result} />;

  // Backend-verified events (a real transcript row core only writes after a
  // genuine dashboard/Discord action) get a compact log-line treatment
  // distinct from the generic prose bubble below — otherwise these are
  // indistinguishable from the model's own narration of the same claim
  // (e.g. "good, user approved" in its own text), which is real but
  // unverified.
  if (entry.type === TranscriptEntryType.APPROVE) {
    return (
      <LogLine badge="approved" badgeClass="success" compact={compact}>
        plan approved — implementing
      </LogLine>
    );
  }
  if (entry.type === TranscriptEntryType.ABORT) {
    return (
      <LogLine badge="killed" badgeClass="warning" compact={compact}>
        {entry.text || "session killed"}
      </LogLine>
    );
  }
  if (entry.type === TranscriptEntryType.INTERRUPT) {
    return (
      <LogLine badge="interrupted" badgeClass="info" compact={compact}>
        {entry.text || "current turn interrupted"}
      </LogLine>
    );
  }
  if (entry.type === TranscriptEntryType.PERMISSION_MODE) {
    return (
      <LogLine badge="mode" badgeClass="info" compact={compact}>
        permission mode → {entry.text}
      </LogLine>
    );
  }

  return (
    <div className={compact ? undefined : "max-w-(--feed-measure)"}>
      <div
        className={`${compact ? "text-sm" : "text-base"} leading-[1.7] ${entry.from === "human" ? "text-primary" : ""}`}
      >
        <Markdown text={asDisplayMarkdown(entry)} />
      </div>
    </div>
  );
}
