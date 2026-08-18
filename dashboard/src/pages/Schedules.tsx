import { useCallback, useEffect, useState } from "react";
import { client } from "../connectClient";
import type { Schedule, Proposal, Repo } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "../components/InlineError";
import { ConfirmModal } from "../components/ConfirmModal";
import { Modal } from "../components/Modal";
import { useMediaQuery } from "../useMediaQuery";

// Scheduled work as a top-level view rather than a header modal (docs/adr/0035
// shipped it as one). A run lands below as a proposal and waits for a human,
// so this page also carries the approve/dismiss for those.
//
// Was "scheduled audits", where the repo was a Go constant (infra-bootstrap)
// and the cadence could only be an interval. Both are data now: a schedule
// targets any repo in the repos table, and its cadence is a cron expression,
// an interval, or a single moment.
//
// Findings and cost are still absent: there is no run-history table, only a
// single last_status string overwritten each tick.

const COLS = "grid-cols-[20px_1fr_150px_190px_110px_150px]";

function humanInterval(seconds: number): string {
  if (seconds % 86400 === 0) {
    const d = seconds / 86400;
    return d === 1 ? "daily" : `every ${d}d`;
  }
  if (seconds % 3600 === 0) {
    const h = seconds / 3600;
    return h === 1 ? "hourly" : `every ${h}h`;
  }
  if (seconds % 60 === 0) return `every ${seconds / 60}m`;
  return `every ${seconds}s`;
}

function relative(iso: string): string {
  if (!iso) return "—";
  const ms = new Date(iso).getTime() - Date.now();
  if (Number.isNaN(ms)) return "—";
  const abs = Math.abs(ms);
  const mins = Math.round(abs / 60_000);
  const unit = mins < 60 ? `${mins}m` : mins < 1440 ? `${Math.round(mins / 60)}h` : `${Math.round(mins / 1440)}d`;
  return ms >= 0 ? `in ${unit}` : `${unit} ago`;
}

// Three cadences, so three branches — humanInterval alone would render a cron
// schedule (whose intervalSeconds is 0) as "every 0d".
function cadence(s: Schedule): string {
  if (s.cron) return s.cron;
  if (s.intervalSeconds) return humanInterval(s.intervalSeconds);
  return "once";
}

type Mode = "cron" | "interval" | "once";

function modeOf(s: Schedule | null): Mode {
  if (!s) return "interval";
  if (s.cron) return "cron";
  if (s.intervalSeconds) return "interval";
  return "once";
}

// <input type="datetime-local"> speaks local wall-clock with no zone; the wire
// wants RFC3339. Native input over a picker library.
function toLocalInput(iso: string): string {
  const d = iso ? new Date(iso) : new Date(Date.now() + 3600_000);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// The full field set every write needs. Update overwrites the whole row, so
// pause/resume has to send the cadence back too — sending only `enabled` is
// how pausing a cron schedule would blank its expression.
function payload(s: Schedule) {
  return {
    id: s.id,
    name: s.name,
    repo: s.repo,
    prompt: s.prompt,
    cron: s.cron,
    intervalSeconds: s.intervalSeconds,
    runAt: s.cron || s.intervalSeconds ? "" : new Date(s.nextRunAt).toISOString(),
  };
}

function ScheduleForm({
  schedule,
  onClose,
  onSaved,
}: {
  // null = creating.
  schedule: Schedule | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(schedule?.name ?? "");
  const [repo, setRepo] = useState(schedule?.repo ?? "");
  const [repos, setRepos] = useState<Repo[]>([]);
  const [prompt, setPrompt] = useState(schedule?.prompt ?? "");
  const [mode, setMode] = useState<Mode>(modeOf(schedule));
  const [cron, setCron] = useState(schedule?.cron ?? "0 9 * * MON");
  const [intervalSeconds, setIntervalSeconds] = useState(schedule?.intervalSeconds || 3600);
  const [runAt, setRunAt] = useState(toLocalInput(schedule?.nextRunAt ?? ""));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // A select, not free text: nothing validates a repo name downstream, so a
  // typo would only surface much later, when the session fails to provision.
  // Fetched from the dashboard-editable repos table (docs/adr/0028).
  useEffect(() => {
    client
      .listRepos({})
      .then((res) => {
        setRepos(res.repos);
        setRepo((current) => current || res.repos[0]?.name || "");
      })
      .catch((err: Error) => setError(err.message));
  }, []);

  async function save() {
    setBusy(true);
    setError(null);
    const cadenceFields = {
      cron: mode === "cron" ? cron.trim() : "",
      intervalSeconds: mode === "interval" ? intervalSeconds : 0,
      runAt: mode === "once" ? new Date(runAt).toISOString() : "",
    };
    try {
      if (schedule) {
        await client.updateSchedule({ id: schedule.id, name, repo, prompt, enabled: schedule.enabled, ...cadenceFields });
      } else {
        await client.createSchedule({ name, repo, prompt, ...cadenceFields });
      }
      onSaved();
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const field =
    "border border-line bg-transparent px-2.5 py-2 text-sm outline-none focus:border-primary/60 placeholder:text-dim2";

  return (
    <Modal open onClose={onClose} boxClassName="max-w-lg">
      <h3 className="text-base font-semibold mb-1">{schedule ? "Edit schedule" : "New schedule"}</h3>
      <p className="text-xs text-dim2 mb-3.5">
        Work an agent runs on a cadence. Each run lands as a proposal you open — it still asks for permission before
        any mutating action.
      </p>
      <div className="flex flex-col gap-2.5">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="name (e.g. weekly-rundown)"
          aria-label="schedule name"
          className={field}
        />
        <label className="flex flex-col gap-1 text-xs text-dim2">
          repo
          <select value={repo} onChange={(e) => setRepo(e.target.value)} aria-label="repo" className={field}>
            {repos.map((r) => (
              <option key={r.name} value={r.name}>
                {r.clusterAccess ? `${r.name} · cluster access` : r.name}
              </option>
            ))}
          </select>
        </label>
        <textarea
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          rows={4}
          placeholder="what should it do?"
          aria-label="schedule prompt"
          className={field}
        />
        <div className="flex gap-3.5 text-xs text-dim">
          {(["cron", "interval", "once"] as Mode[]).map((m) => (
            <label key={m} className="flex items-center gap-1.5 cursor-pointer">
              <input type="radio" name="mode" checked={mode === m} onChange={() => setMode(m)} aria-label={m} />
              {m}
            </label>
          ))}
        </div>
        {mode === "cron" && (
          <div className="flex flex-col gap-1">
            <input value={cron} onChange={(e) => setCron(e.target.value)} aria-label="cron expression" className={field} />
            <span className="text-xs text-dim2">
              5-field cron. Prefix <code>CRON_TZ=Europe/Paris </code> to pin a timezone — without one it is UTC.
            </span>
          </div>
        )}
        {mode === "interval" && (
          <div className="flex items-center gap-2.5">
            <input
              type="number"
              min={60}
              value={intervalSeconds}
              onChange={(e) => setIntervalSeconds(Number(e.target.value))}
              aria-label="interval in seconds"
              className={`w-28 ${field}`}
            />
            <span className="text-xs text-dim2">
              seconds between runs — {humanInterval(Math.max(60, intervalSeconds))} (minimum 60)
            </span>
          </div>
        )}
        {mode === "once" && (
          <div className="flex items-center gap-2.5">
            <input
              type="datetime-local"
              value={runAt}
              onChange={(e) => setRunAt(e.target.value)}
              aria-label="run at"
              className={field}
            />
            <span className="text-xs text-dim2">fires once, then pauses itself</span>
          </div>
        )}
        {error && <div className="text-sm text-error">{error}</div>}
        <div className="flex gap-2.5 mt-1">
          <button
            type="button"
            disabled={
              busy ||
              !name.trim() ||
              !prompt.trim() ||
              !repo ||
              (mode === "interval" && intervalSeconds < 60) ||
              (mode === "cron" && !cron.trim()) ||
              (mode === "once" && !runAt)
            }
            onClick={() => void save()}
            className="bg-primary text-primary-content px-4 py-2 text-sm font-semibold disabled:opacity-50"
          >
            {schedule ? "Save" : "Create"}
          </button>
          <button type="button" onClick={onClose} className="border border-line px-4 py-2 text-sm text-dim">
            Cancel
          </button>
        </div>
      </div>
    </Modal>
  );
}

function StatusDot({ schedule }: { schedule: Schedule }) {
  const failed = schedule.lastStatus.startsWith("error");
  const cls = !schedule.enabled ? "border border-dim2" : failed ? "bg-warning" : "bg-success";
  return <span className={`w-[7px] h-[7px] rounded-full flex-none ${cls}`} />;
}
function scheduleActions(
  s: Schedule,
  act: (fn: () => Promise<unknown>) => void,
  onEdit: () => void,
  onDelete: () => void,
  busy: boolean,
) {
  const btn = "border border-line px-2.5 py-1 text-xs text-dim hover:text-base-content disabled:opacity-50";
  return (
    <>
      <button type="button" disabled={busy} onClick={() => act(() => client.runScheduleNow({ id: s.id }))} className={btn}>
        run now
      </button>
      <button type="button" onClick={onEdit} className={btn}>
        edit
      </button>
      <button
        type="button"
        disabled={busy}
        onClick={() => act(() => client.updateSchedule({ ...payload(s), enabled: !s.enabled }))}
        className={btn}
      >
        {s.enabled ? "pause" : "resume"}
      </button>
      <button
        type="button"
        onClick={onDelete}
        className="border border-line px-2.5 py-1 text-xs text-dim hover:text-error"
      >
        delete
      </button>
    </>
  );
}

export function Schedules({
  proposals,
  onSelectSession,
  reloadSessions,
}: {
  // Rows from the `proposals` table, not sessions. A proposal has no pod, no
  // transcript and no worktree until a human opens it — which is the point:
  // it is the gate in front of a machine-initiated run (docs/adr/0048).
  proposals: Proposal[];
  onSelectSession: (id: string) => void;
  reloadSessions: () => void;
}) {
  const isDesktop = useMediaQuery("(min-width: 640px)");
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState<Schedule | null>(null);
  const [creating, setCreating] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<Schedule | null>(null);

  const load = useCallback(() => {
    setLoading(true);
    return client
      .listSchedules({})
      .then((res) => setSchedules(res.schedules))
      .catch((err: Error) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const act = (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    fn()
      .then(() => load())
      .catch((err: Error) => setError(err.message))
      .finally(() => setBusy(false));
  };

  const proposeAct = (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    fn()
      .then(() => reloadSessions())
      .catch((err: Error) => setError(err.message))
      .finally(() => setBusy(false));
  };

  return (
    <div className="flex-1 min-h-0 overflow-y-auto px-3.5 sm:px-4.5 pt-4 sm:pt-5 pb-6 flex flex-col gap-3.5">
      <InlineError message={error} onRetry={load} onDismiss={() => setError(null)} />
      {(creating || editing) && (
        <ScheduleForm schedule={editing} onClose={() => { setCreating(false); setEditing(null); }} onSaved={load} />
      )}
      <ConfirmModal
        open={pendingDelete !== null}
        title="Delete schedule?"
        message={`"${pendingDelete?.name ?? ""}" will stop running. This cannot be undone.`}
        confirmLabel="Delete"
        onConfirm={() => {
          const a = pendingDelete;
          setPendingDelete(null);
          if (a) act(() => client.deleteSchedule({ id: a.id }));
        }}
        onCancel={() => setPendingDelete(null)}
      />

      <div className="flex items-baseline gap-3 flex-wrap">
        <h2 className="text-base font-semibold">Schedules</h2>
        {isDesktop && (
          <span className="text-xs text-dim2">
            cron, interval or one-shot · a run lands below as a <span className="text-primary">proposal</span> and waits for
            you to open it as a session
          </span>
        )}
        <button
          type="button"
          onClick={() => setCreating(true)}
          className="ml-auto border border-acc-line px-3 py-1.5 text-xs hover:border-primary hover:text-primary"
        >
          + new schedule
        </button>
      </div>

      {/*
        Proposals aren't audit rows — they're this page's real to-do.

        This block never rendered: App.tsx fed it a permanently-empty memo left
        over from when a proposal was a session row with status='proposed'. So the
        entire human gate in front of machine-initiated runs was unreachable
        from the UI, and the two RPCs behind it had no caller. It now reads the
        `proposals` table via ListProposals.
      */}
      {proposals.length > 0 && (
        <div className="border border-acc-line bg-pink-bg px-3.5 py-3">
          <div className="flex items-center gap-2.5 flex-wrap">
            <span className="w-[7px] h-[7px] rounded-full bg-primary flex-none" />
            <span className="text-sm">
              {proposals.length} proposed run{proposals.length === 1 ? "" : "s"} waiting for approval
            </span>
          </div>
          <div className="flex flex-col gap-2.5 mt-2.5">
            {proposals.map((p) => (
              <div key={p.id} className="flex flex-col gap-1">
                <div className="flex items-center gap-2.5 flex-wrap">
                  <span className="text-sm min-w-0 truncate flex-1">
                    <span className="text-dim2">{p.repo}</span> {p.title}
                  </span>
                  <span className="flex-none text-xs text-dim2 border border-line px-1.5">{p.source}</span>
                  <button
                    type="button"
                    disabled={busy}
                    // THE human gate: the one call that can hand a
                    // cluster-access agent a pod. It creates the session and
                    // sends the proposal body as its first message, so the
                    // agent's instruction is the text shown here — no wrapper.
                    onClick={() =>
                      proposeAct(async () => {
                        const res = await client.openFromProposal({ proposalId: p.id });
                        if (res.session) onSelectSession(res.session.id);
                      })
                    }
                    className="flex-none bg-primary text-primary-content px-3.5 py-1.5 text-sm font-semibold disabled:opacity-50"
                  >
                    approve &amp; dispatch
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    // dismissProposal, not deleteSession — this used to pass a
                    // proposal id as a sessionId, which could only ever 404
                    // (or, worse, match an unrelated session).
                    onClick={() => proposeAct(() => client.dismissProposal({ proposalId: p.id }))}
                    className="flex-none border border-acc-line px-3.5 py-1.5 text-sm hover:border-error hover:text-error disabled:opacity-50"
                  >
                    dismiss
                  </button>
                </div>
                {p.body && (
                  <div className="text-xs text-dim whitespace-pre-wrap break-words line-clamp-4">{p.body}</div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {loading && schedules.length === 0 && <div className="text-sm text-dim">Loading…</div>}
      {!loading && schedules.length === 0 && <div className="text-sm text-dim2">Nothing scheduled.</div>}

      {schedules.length > 0 &&
        (isDesktop ? (
          <div className="border border-line2">
            <div className={`grid ${COLS} gap-3.5 px-3.5 py-2 border-b border-line text-2xs tracking-[0.1em] text-dim2`}>
              <div />
              <div>SCHEDULE</div>
              <div>CADENCE</div>
              <div>LAST RUN</div>
              <div>NEXT</div>
              <div />
            </div>
            {schedules.map((a, i) => (
              <div
                key={a.id}
                className={`grid ${COLS} gap-3.5 px-3.5 py-2.5 items-center ${
                  i === schedules.length - 1 ? "" : "border-b border-line3"
                } ${a.lastStatus.startsWith("error") ? "bg-orange-wash" : ""}`}
              >
                <StatusDot schedule={a} />
                <div className="min-w-0">
                  <div className="text-base truncate">
                    {a.name} <span className="text-xs text-dim2">{a.repo}</span>
                  </div>
                  <div className="text-xs text-dim2 mt-0.5 truncate" title={a.prompt}>
                    {a.prompt.split("\n")[0]}
                  </div>
                </div>
                <div className="text-sm text-dim">{cadence(a)}</div>
                <div className="min-w-0">
                  {a.lastRunAt ? (
                    <>
                      <div className="text-sm text-text2">{relative(a.lastRunAt)}</div>
                      <div
                        className={`text-xs mt-0.5 truncate ${a.lastStatus.startsWith("error") ? "text-warning" : "text-dim2"}`}
                        title={a.lastStatus}
                      >
                        {a.lastStatus || "no status"}
                      </div>
                    </>
                  ) : (
                    <span className="text-sm text-dim2">never</span>
                  )}
                </div>
                <div className="text-sm text-dim">{a.enabled ? relative(a.nextRunAt) : "paused"}</div>
                <div className="flex gap-1.5 justify-end flex-wrap">
                  {scheduleActions(a, act, () => setEditing(a), () => setPendingDelete(a), busy)}
                </div>
              </div>
            ))}
          </div>
        ) : (
          schedules.map((a) => (
            <div
              key={a.id}
              className={`border px-3.5 py-3 ${
                a.lastStatus.startsWith("error") ? "border-orange-line bg-orange-bg" : "border-line2"
              }`}
            >
              <div className="flex items-center gap-2">
                <StatusDot schedule={a} />
                <span className="text-sm min-w-0 truncate">
                  {a.name} <span className="text-xs text-dim2">{a.repo}</span>
                </span>
                <span className="text-xs text-dim2 ml-auto flex-none">
                  {a.enabled ? relative(a.nextRunAt) : "paused"}
                </span>
              </div>
              <div className="text-xs text-dim mt-1.5 leading-[1.6]">
                {cadence(a)}
                <br />
                <span className="text-dim2">{a.prompt.split("\n")[0]}</span>
              </div>
              <div className={`text-xs mt-1.5 ${a.lastStatus.startsWith("error") ? "text-warning" : "text-text2"}`}>
                {a.lastRunAt ? `last: ${relative(a.lastRunAt)} · ${a.lastStatus || "no status"}` : "never run"}
              </div>
              <div className="flex gap-2 mt-2.5 flex-wrap">
                {scheduleActions(a, act, () => setEditing(a), () => setPendingDelete(a), busy)}
              </div>
            </div>
          ))
        ))}
    </div>
  );
}
