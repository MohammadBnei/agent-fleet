import { useCallback, useEffect, useState } from "react";
import { client } from "../connectClient";
import { ConfirmModal } from "./ConfirmModal";
import type { ScheduledAudit } from "../gen/agentfleet/v1/dashboard_pb";

// Mirrors ManageReposModal's pattern (docs/adr/0028's "dashboard-editable
// entity, no redeploy" idiom): a modal opened from a header button, an
// inline create form, per-row dirty tracking with its own Save, and a
// SIBLING confirm modal — a <dialog> nested inside an open <dialog>
// closes both on Cancel, which that file already learned the hard way.

function AuditRow({ audit, onChanged }: { audit: ScheduledAudit; onChanged: () => void }) {
  const [name, setName] = useState(audit.name);
  const [prompt, setPrompt] = useState(audit.prompt);
  const [intervalSeconds, setIntervalSeconds] = useState(audit.intervalSeconds);
  const [enabled, setEnabled] = useState(audit.enabled);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const dirty =
    name !== audit.name ||
    prompt !== audit.prompt ||
    intervalSeconds !== audit.intervalSeconds ||
    enabled !== audit.enabled;

  async function save() {
    setBusy(true);
    setError(null);
    try {
      await client.updateScheduledAudit({ id: audit.id, name, prompt, intervalSeconds, enabled });
      onChanged();
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="border border-base-content/10 rounded-lg p-3 flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="input input-sm input-bordered flex-1"
          placeholder="name"
        />
        <label className="flex items-center gap-1 text-[11px]">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="checkbox checkbox-xs"
          />
          enabled
        </label>
      </div>
      <textarea
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        className="textarea textarea-sm textarea-bordered w-full"
        rows={2}
        placeholder="what should thot check?"
      />
      <div className="flex items-center gap-2">
        <input
          type="number"
          min={60}
          value={intervalSeconds}
          onChange={(e) => setIntervalSeconds(Number(e.target.value))}
          className="input input-sm input-bordered w-32"
        />
        <span className="text-[11px] text-base-content/50">seconds between runs</span>
        <button
          type="button"
          className="btn btn-sm btn-primary ml-auto"
          disabled={!dirty || busy}
          onClick={() => void save()}
        >
          Save
        </button>
      </div>
      {audit.lastRunAt && (
        <p className="text-[10.5px] text-base-content/40">
          last run {audit.lastRunAt} — {audit.lastStatus || "no status"}
        </p>
      )}
      {error && <p className="text-error text-[11px]">{error}</p>}
    </div>
  );
}

export function ManageScheduledAuditsModal() {
  const [open, setOpen] = useState(false);
  const [audits, setAudits] = useState<ScheduledAudit[]>([]);
  const [name, setName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [intervalSeconds, setIntervalSeconds] = useState(3600);
  const [pendingDelete, setPendingDelete] = useState<ScheduledAudit | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await client.listScheduledAudits({});
      setAudits(res.audits);
      setError(null);
    } catch (err) {
      setError(String(err));
    }
  }, []);

  // Fetch on open, not eagerly — same as ManageReposModal.
  useEffect(() => {
    if (open) void load();
  }, [open, load]);

  async function create() {
    try {
      await client.createScheduledAudit({ name, prompt, intervalSeconds });
      setName("");
      setPrompt("");
      await load();
    } catch (err) {
      setError(String(err));
    }
  }

  async function confirmDelete() {
    if (!pendingDelete) return;
    try {
      await client.deleteScheduledAudit({ id: pendingDelete.id });
      setPendingDelete(null);
      await load();
    } catch (err) {
      setError(String(err));
      setPendingDelete(null);
    }
  }

  return (
    <>
      <button type="button" className="btn btn-ghost btn-xs" onClick={() => setOpen(true)}>
        Audits
      </button>

      {open && (
        <dialog className="modal modal-open">
          <div className="modal-box max-w-2xl">
            <h3 className="font-semibold mb-3">Scheduled audits</h3>
            <p className="text-[11px] text-base-content/50 mb-4">
              Periodic cluster checks thot runs on its own. Each run still asks for
              permission before any mutating action.
            </p>

            {error && <p className="text-error text-[12px] mb-2">{error}</p>}

            <div className="flex flex-col gap-3 mb-4 max-h-[45vh] overflow-y-auto">
              {audits.length === 0 && (
                <p className="text-[12px] text-base-content/50">No audits scheduled.</p>
              )}
              {audits.map((a) => (
                <div key={a.id} className="flex gap-2 items-start">
                  <div className="flex-1 min-w-0">
                    <AuditRow audit={a} onChanged={() => void load()} />
                  </div>
                  <button
                    type="button"
                    className="btn btn-ghost btn-xs text-error"
                    onClick={() => setPendingDelete(a)}
                  >
                    ✕
                  </button>
                </div>
              ))}
            </div>

            <div className="border-t border-base-content/10 pt-3 flex flex-col gap-2">
              <div className="text-[11px] font-semibold">New audit</div>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                className="input input-sm input-bordered"
                placeholder="name (e.g. etcd-health)"
              />
              <textarea
                value={prompt}
                onChange={(e) => setPrompt(e.target.value)}
                className="textarea textarea-sm textarea-bordered"
                rows={2}
                placeholder="what should thot check?"
              />
              <div className="flex items-center gap-2">
                <input
                  type="number"
                  min={60}
                  value={intervalSeconds}
                  onChange={(e) => setIntervalSeconds(Number(e.target.value))}
                  className="input input-sm input-bordered w-32"
                />
                <span className="text-[11px] text-base-content/50">seconds</span>
                <button
                  type="button"
                  className="btn btn-sm btn-primary ml-auto"
                  disabled={!name || !prompt || intervalSeconds < 60}
                  onClick={() => void create()}
                >
                  Add
                </button>
              </div>
            </div>

            <div className="modal-action">
              <button type="button" className="btn btn-sm" onClick={() => setOpen(false)}>
                Close
              </button>
            </div>
          </div>
        </dialog>
      )}

      {/* Sibling, never nested inside the dialog above. */}
      <ConfirmModal
        open={pendingDelete !== null}
        title="Delete scheduled audit?"
        message={`"${pendingDelete?.name ?? ""}" will stop running. This cannot be undone.`}
        confirmLabel="Delete"
        onConfirm={() => void confirmDelete()}
        onCancel={() => setPendingDelete(null)}
      />
    </>
  );
}
