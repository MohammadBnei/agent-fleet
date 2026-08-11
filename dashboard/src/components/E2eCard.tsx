import type { GetE2eStatusResponse } from "../gen/agentfleet/v1/dashboard_pb";

// Deliberately not a Panel: fixed, small, and always at the top of the
// sidebar. Panels are the resizable/collapsible feed surfaces (todos, tool
// calls, changes) and carry the fit-height machinery with them; this is a
// status readout, not a feed.
export function E2eCard({ e2e }: { e2e: GetE2eStatusResponse | null }) {
  if (!e2e || !e2e.status) return null;

  // Three states, and the middle one is the whole point of this card: a pod
  // can sit at Running for 13 minutes doing a cold `bun install`, or sit
  // there forever because the start command bound 127.0.0.1 instead of
  // 0.0.0.0. Before the AppPort readiness probe those were indistinguishable
  // and the preview just returned 502 with nothing to explain it.
  const running = e2e.podPhase === "Running";
  const badge = e2e.appReady
    ? { text: "serving", cls: "text-success border-success/40 bg-success/10" }
    : running
      ? { text: "starting", cls: "text-warning border-warning/40 bg-warning/10" }
      : { text: e2e.podPhase.toLowerCase() || e2e.status, cls: "text-base-content/50 border-base-content/20" };

  const ingredients = [...e2e.tools, ...e2e.services];

  return (
    // min-w-0 on the root and on every shrinkable child is load-bearing, not
    // decoration: `truncate` implies white-space:nowrap, which makes the
    // preview URL's min-content width the *whole* URL. Without min-w-0 a
    // flex item refuses to shrink below that, and on mobile (390px) this
    // card silently widened the entire column to 437px — caught by
    // measuring rects in Playwright, invisible to tsc and to lint.
    <div className="rounded-lg border border-base-content/10 bg-base-300/20 p-1.5 flex flex-col gap-1 min-w-0">
      <div className="flex items-center gap-1.5 min-w-0">
        <div className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold flex-1 min-w-0">E2E ENV</div>
        {e2e.startCmdOverridden && (
          <span
            title="A human-approved override is running for this task only; the repo's profile is unchanged."
            className="flex-none px-1 py-0.5 rounded text-[9px] font-semibold border tracking-wide text-warning border-warning/40 bg-warning/10"
          >
            overridden
          </span>
        )}
        <span className={`flex-none px-1.5 py-0.5 rounded text-[9px] font-semibold border tracking-wide ${badge.cls}`}>
          {badge.text}
        </span>
      </div>

      {e2e.previewUrl && e2e.appReady && (
        <a
          href={e2e.previewUrl}
          target="_blank"
          rel="noreferrer"
          title={e2e.previewUrl}
          className="text-[10.5px] text-primary hover:underline truncate min-w-0"
        >
          {e2e.previewUrl}
        </a>
      )}

      {running && !e2e.appReady && (
        <div className="text-[10px] text-base-content/50 leading-snug min-w-0">
          Nothing on the app port yet — installing, or the command never binds <span className="text-base-content/70">0.0.0.0:$PORT</span>.
        </div>
      )}

      <div className="flex items-baseline gap-1.5 min-w-0">
        <span className="text-[9px] text-base-content/40 flex-none">
          {e2e.profileName ? `profile: ${e2e.profileName}` : "no profile"}
        </span>
        {e2e.restarts > 0 && (
          <span className="text-[9px] text-warning flex-none">
            · {e2e.restarts} restart{e2e.restarts === 1 ? "" : "s"}
          </span>
        )}
      </div>

      {e2e.startCmd && (
        // overflow-wrap:anywhere, not break-all: `anywhere` still breaks a
        // single unbreakable token (so the box can shrink) but prefers
        // whitespace, where break-all produced "bunx prisma mig/rate deploy".
        <code className="text-[9.5px] text-base-content/70 bg-base-content/5 rounded px-1 py-0.5 leading-snug min-w-0 [overflow-wrap:anywhere]">
          {e2e.startCmd}
        </code>
      )}

      {ingredients.length > 0 && (
        <div className="flex flex-wrap gap-1 min-w-0">
          {ingredients.map((i) => (
            <span key={i} className="px-1 py-0.5 rounded text-[9px] border border-base-content/15 text-base-content/50">
              {i}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
