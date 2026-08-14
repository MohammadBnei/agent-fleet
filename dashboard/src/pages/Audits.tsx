import { useCallback, useEffect, useState } from "react";
import { client } from "../connectClient";
import type { ScheduledAudit } from "../gen/agentfleet/v1/dashboard_pb";
import type { Session } from "../gen/agentfleet/v1/core_pb";
import { InlineError } from "../components/InlineError";
import { ConfirmModal } from "../components/ConfirmModal";
import { Modal } from "../components/Modal";
import { useMediaQuery } from "../useMediaQuery";

// Scheduled audits as a top-level view rather than a header modal (docs/adr/
// 0035 shipped them as one). A run lands in the task list as `proposed` and
// waits for a human, so this page also carries the approve/dismiss for those.
//
// Schedule is an interval, NOT cron: db/migrations/000006 records the reason —
// no cron parser exists anywhere in this codebase, and a cron column can be
// added later without touching that one. The mockups draw `0 3 * * *`; this
// renders "every 6h" from interval_seconds instead of implying a parser exists.
// Findings and cost are likewise absent: there is no run-history table, only a
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

function AuditForm({
  audit,
  onClose,
  onSaved,
}: {
  // null = creating.
  audit: ScheduledAudit | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(audit?.name ?? "");
  const [prompt, setPrompt] = useState(audit?.prompt ?? "");
  const [intervalSeconds, setIntervalSeconds] = useState(audit?.intervalSeconds ?? 3600);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function save() {
    setBusy(true);
    setError(null);
    try {
      if (audit) {
        await client.updateScheduledAudit({ id: audit.id, name, prompt, intervalSeconds, enabled: audit.enabled });
      } else {
        await client.createScheduledAudit({ name, prompt, intervalSeconds });
      }
      onSaved();
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal open onClose={onClose} boxClassName="max-w-lg">
      <h3 className="text-base font-semibold mb-1">{audit ? "Edit audit" : "New audit"}</h3>
      <p className="text-xs text-dim2 mb-3.5">
        A periodic check the cluster agent runs on its own. Each run still asks for permission before any mutating
        action, and lands as a proposal you approve.
      </p>
      <div className="flex flex-col gap-2.5">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="name (e.g. etcd-health)"
          aria-label="audit name"
          className="border border-line bg-transparent px-2.5 py-2 text-sm outline-none focus:border-primary/60 placeholder:text-dim2"
        />
        <textarea
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          rows={4}
          placeholder="what should it check?"
          aria-label="audit prompt"
          className="border border-line bg-transparent px-2.5 py-2 text-sm outline-none focus:border-primary/60 placeholder:text-dim2"
        />
        <div className="flex items-center gap-2.5">
          <input
            type="number"
            min={60}
            value={intervalSeconds}
            onChange={(e) => setIntervalSeconds(Number(e.target.value))}
            aria-label="interval in seconds"
            className="w-28 border border-line bg-transparent px-2.5 py-2 text-sm outline-none focus:border-primary/60"
          />
          <span className="text-xs text-dim2">
            seconds between runs — {humanInterval(Math.max(60, intervalSeconds))} (minimum 60)
          </span>
        </div>
        {error && <div className="text-sm text-error">{error}</div>}
        <div className="flex gap-2.5 mt-1">
          <button
            type="button"
            disabled={busy || !name.trim() || !prompt.trim() || intervalSeconds < 60}
            onClick={() => void save()}
            className="bg-primary text-primary-content px-4 py-2 text-sm font-semibold disabled:opacity-50"
          >
            {audit ? "Save" : "Create"}
          </button>
          <button type="button" onClick={onClose} className="border border-line px-4 py-2 text-sm text-dim">
            Cancel
          </button>
        </div>
      </div>
    </Modal>
  );
}

function StatusDot({ audit }: { audit: ScheduledAudit }) {
  const failed = audit.lastStatus.startsWith("error");
  const cls = !audit.enabled ? "border border-dim2" : failed ? "bg-warning" : "bg-success";
  return <span className={`w-[7px] h-[7px] rounded-full flex-none ${cls}`} />;
}

function auditActions(
  audit: ScheduledAudit,
  act: (fn: () => Promise<unknown>) => void,
  onEdit: () => void,
  onDelete: () => void,
  busy: boolean,
) {
  const btn = "border border-line px-2.5 py-1 text-xs text-dim hover:text-base-content disabled:opacity-50";
  return (
    <>
      <button type="button" disabled={busy} onClick={() => act(() => client.runScheduledAuditNow({ id: audit.id }))} className={btn}>
        run now
      </button>
      <button type="button" onClick={onEdit} className={btn}>
        edit
      </button>
      <button
        type="button"
        disabled={busy}
        onClick={() =>
          act(() =>
            client.updateScheduledAudit({
              id: audit.id,
              name: audit.name,
              prompt: audit.prompt,
              intervalSeconds: audit.intervalSeconds,
              enabled: !audit.enabled,
            }),
          )
        }
        className={btn}
      >
        {audit.enabled ? "pause" : "resume"}
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

export function Audits({
  proposed,
  onSelectTask,
  reloadTasks,
}: {
  proposed: Session[];
  onSelectTask: (id: string) => void;
  reloadTasks: () => void;
}) {
  const isDesktop = useMediaQuery("(min-width: 640px)");
  const [audits, setAudits] = useState<ScheduledAudit[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState<ScheduledAudit | null>(null);
  const [creating, setCreating] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<ScheduledAudit | null>(null);

  const load = useCallback(() => {
    setLoading(true);
    return client
      .listScheduledAudits({})
      .then((res) => setAudits(res.audits))
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
      .then(() => reloadTasks())
      .catch((err: Error) => setError(err.message))
      .finally(() => setBusy(false));
  };

  return (
    <div className="flex-1 min-h-0 overflow-y-auto px-3.5 sm:px-4.5 pt-4 sm:pt-5 pb-6 flex flex-col gap-3.5">
      <InlineError message={error} onRetry={load} onDismiss={() => setError(null)} />
      {(creating || editing) && (
        <AuditForm audit={editing} onClose={() => { setCreating(false); setEditing(null); }} onSaved={load} />
      )}
      <ConfirmModal
        open={pendingDelete !== null}
        title="Delete scheduled audit?"
        message={`"${pendingDelete?.name ?? ""}" will stop running. This cannot be undone.`}
        confirmLabel="Delete"
        onConfirm={() => {
          const a = pendingDelete;
          setPendingDelete(null);
          if (a) act(() => client.deleteScheduledAudit({ id: a.id }));
        }}
        onCancel={() => setPendingDelete(null)}
      />

      <div className="flex items-baseline gap-3 flex-wrap">
        <h2 className="text-base font-semibold">Scheduled audits</h2>
        {isDesktop && (
          <span className="text-xs text-dim2">
            recurring tasks · a run lands in the task list as <span className="text-primary">proposed</span> and waits
            for you
          </span>
        )}
        <button
          type="button"
          onClick={() => setCreating(true)}
          className="ml-auto border border-acc-line px-3 py-1.5 text-xs hover:border-primary hover:text-primary"
        >
          + new audit
        </button>
      </div>

      {/* Proposals aren't audit rows — they're this page's real to-do. */}
      {proposed.length > 0 && (
        <div className="border border-acc-line bg-pink-bg px-3.5 py-3">
          <div className="flex items-center gap-2.5 flex-wrap">
            <span className="w-[7px] h-[7px] rounded-full bg-primary flex-none" />
            <span className="text-sm">
              {proposed.length} proposed run{proposed.length === 1 ? "" : "s"} waiting for approval
            </span>
          </div>
          <div className="flex flex-col gap-2 mt-2.5">
            {proposed.map((t) => (
              <div key={t.id} className="flex items-center gap-2.5 flex-wrap">
                <button
                  type="button"
                  onClick={() => onSelectTask(t.id)}
                  className="text-sm text-dim hover:text-primary text-left min-w-0 truncate flex-1"
                >
                  #{t.id.slice(0, 6)} {t.description}
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => proposeAct(() => client.openFromProposal({ proposalId: t.id }))}
                  className="flex-none bg-primary text-primary-content px-3.5 py-1.5 text-sm font-semibold disabled:opacity-50"
                >
                  approve &amp; dispatch
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => proposeAct(() => client.deleteSession({ sessionId: t.id }))}
                  className="flex-none border border-acc-line px-3.5 py-1.5 text-sm hover:border-error hover:text-error disabled:opacity-50"
                >
                  dismiss
                </button>
              </div>
            ))}
          </div>
        </div>
      )}

      {loading && audits.length === 0 && <div className="text-sm text-dim">Loading…</div>}
      {!loading && audits.length === 0 && <div className="text-sm text-dim2">No audits scheduled.</div>}

      {audits.length > 0 &&
        (isDesktop ? (
          <div className="border border-line2">
            <div className={`grid ${COLS} gap-3.5 px-3.5 py-2 border-b border-line text-2xs tracking-[0.1em] text-dim2`}>
              <div />
              <div>AUDIT</div>
              <div>SCHEDULE</div>
              <div>LAST RUN</div>
              <div>NEXT</div>
              <div />
            </div>
            {audits.map((a, i) => (
              <div
                key={a.id}
                className={`grid ${COLS} gap-3.5 px-3.5 py-2.5 items-center ${
                  i === audits.length - 1 ? "" : "border-b border-line3"
                } ${a.lastStatus.startsWith("error") ? "bg-orange-wash" : ""}`}
              >
                <StatusDot audit={a} />
                <div className="min-w-0">
                  <div className="text-base truncate">{a.name}</div>
                  <div className="text-xs text-dim2 mt-0.5 truncate" title={a.prompt}>
                    {a.prompt.split("\n")[0]}
                  </div>
                </div>
                <div className="text-sm text-dim">{humanInterval(a.intervalSeconds)}</div>
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
                  {auditActions(a, act, () => setEditing(a), () => setPendingDelete(a), busy)}
                </div>
              </div>
            ))}
          </div>
        ) : (
          audits.map((a) => (
            <div
              key={a.id}
              className={`border px-3.5 py-3 ${
                a.lastStatus.startsWith("error") ? "border-orange-line bg-orange-bg" : "border-line2"
              }`}
            >
              <div className="flex items-center gap-2">
                <StatusDot audit={a} />
                <span className="text-sm min-w-0 truncate">{a.name}</span>
                <span className="text-xs text-dim2 ml-auto flex-none">
                  {a.enabled ? relative(a.nextRunAt) : "paused"}
                </span>
              </div>
              <div className="text-xs text-dim mt-1.5 leading-[1.6]">
                {humanInterval(a.intervalSeconds)}
                <br />
                <span className="text-dim2">{a.prompt.split("\n")[0]}</span>
              </div>
              <div className={`text-xs mt-1.5 ${a.lastStatus.startsWith("error") ? "text-warning" : "text-text2"}`}>
                {a.lastRunAt ? `last: ${relative(a.lastRunAt)} · ${a.lastStatus || "no status"}` : "never run"}
              </div>
              <div className="flex gap-2 mt-2.5 flex-wrap">
                {auditActions(a, act, () => setEditing(a), () => setPendingDelete(a), busy)}
              </div>
            </div>
          ))
        ))}
    </div>
  );
}
