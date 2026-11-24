import { useEffect, useState } from "react";
import { client } from "../connectClient";
import type { AgentDefinition } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "./InlineError";

// Defined agents' models (docs/adr/0088 amendment). The definition's file in
// git is the default and the bound; an admin's pick is stored in the database
// and applies to the next session (a standing one: its next start). A running
// session never changes model.

// Where the model a new session gets comes from.
export function modelSource(d: AgentDefinition): string {
  if (d.overrideModel !== "" && d.overrideIgnored === "") {
    const when = d.overrideAt ? ` ${d.overrideAt.slice(0, 10)}` : "";
    return `override by ${d.overrideBy}${when}`;
  }
  return d.yamlModel !== "" ? "yaml" : "Claude Code's pick";
}

// A running standing session keeps its model until it is restarted.
export function appliesOnRestart(d: AgentDefinition): boolean {
  return d.standing && d.running && d.runningModel !== d.model;
}

const pick = (m: string) => (m !== "" ? m : "Claude Code's pick");

export function AgentRow({
  d,
  busy,
  onPick,
}: {
  d: AgentDefinition;
  busy: boolean;
  onPick: (model: string) => void;
}) {
  const choices = d.choices.filter((c) => c.unavailableReason === "");
  const unavailable = d.choices.filter((c) => c.unavailableReason !== "");
  return (
    <div className="py-2 border-b border-base-content/5 last:border-0">
      <div className="flex items-center gap-2 flex-wrap">
        <span className="text-sm font-medium w-44 truncate flex-none">
          {d.name}
        </span>
        <code className="text-xs flex-1 min-w-0 break-all">
          {d.model !== "" ? d.model : "—"}
        </code>
        <span className="text-xs text-dim flex-none">{modelSource(d)}</span>
        {d.valid && (
          <>
            <select
              aria-label={`model for ${d.name}`}
              value={d.overrideIgnored === "" ? d.overrideModel : ""}
              disabled={busy}
              onChange={(e) => onPick(e.target.value)}
              className="select select-xs select-bordered"
            >
              <option value="">
                {d.yamlModel !== "" ? "yaml default" : "Claude Code's pick"}
              </option>
              {choices.map((c) => (
                <option key={c.model} value={c.model}>
                  {c.model}
                </option>
              ))}
            </select>
            <button
              type="button"
              disabled={busy || d.overrideModel === ""}
              onClick={() => onPick("")}
              className="border border-line px-3 py-1 text-xs hover:border-error hover:text-error disabled:opacity-50 cursor-pointer"
            >
              clear
            </button>
          </>
        )}
      </div>
      {!d.valid && <div className="text-xs text-error mt-1">{d.error}</div>}
      {d.overrideIgnored !== "" && (
        <div className="text-xs text-warning mt-1">
          Override {d.overrideModel} is ignored: {d.overrideIgnored}
        </div>
      )}
      {appliesOnRestart(d) && (
        <div className="text-xs text-dim mt-1">
          Running on {pick(d.runningModel)}; {pick(d.model)} applies on restart.
        </div>
      )}
      {unavailable.length > 0 && (
        <details className="mt-1">
          <summary className="text-2xs text-dim2 cursor-pointer">
            {unavailable.length} not available
          </summary>
          <ul className="text-2xs text-dim">
            {unavailable.map((c) => (
              <li key={c.model}>
                <code>{c.model}</code>: {c.unavailableReason}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  );
}

export function AgentsPanel() {
  const [defs, setDefs] = useState<AgentDefinition[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const load = () =>
    client.listDefinitions({}).then(
      (r) => setDefs(r.definitions),
      (e: unknown) => setError(String(e)),
    );
  useEffect(() => {
    void load();
  }, []);

  const choose = async (d: AgentDefinition, model: string) => {
    setBusy(d.name);
    setError(null);
    try {
      await client.setDefinitionModel({ definition: d.name, model });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
    await load();
  };

  return (
    <div>
      <p className="text-xs text-dim mb-3">
        The model a new session of each defined agent gets. Everything else
        about a definition stays in its file; a pick can only narrow what the
        file allows. A session that is already running keeps its model; a
        standing one takes the new one when it is restarted.
      </p>
      {error && <InlineError message={error} />}
      {defs === null ? (
        <div className="text-xs text-dim">loading…</div>
      ) : defs.length === 0 ? (
        <div className="text-xs text-dim">No defined agents.</div>
      ) : (
        defs.map((d) => (
          <AgentRow
            key={d.name}
            d={d}
            busy={busy !== null}
            onPick={(m) => void choose(d, m)}
          />
        ))
      )}
    </div>
  );
}
