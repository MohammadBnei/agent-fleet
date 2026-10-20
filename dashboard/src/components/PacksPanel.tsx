import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { ConfirmModal } from "./ConfirmModal";
import type { Pack, PackAttachment } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "./InlineError";

// Live context packs (docs/adr/0063): instructions and skills that load into
// every start of the agents and repos that use them. Never sent as a
// message. A list, then one focused editor per pack: everything in the editor
// is staged and applied by its one Save (a new version if the text or skills
// changed, then the "used by" changes). Fleet-wide packs (packs/fleet,
// packs/repo) change by pull request and are not shown here.

type Targets = { definition: string[]; repo: string[] };
type SkillDraft = { name: string; description: string; body: string };
type Use = { kind: string; target: string };

const keyOf = (u: Use) => `${u.kind}/${u.target}`;
const errText = (err: unknown) => (err instanceof Error ? err.message : String(err));
const sameJSON = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

function SkillEditor({ skill, onChange, onDone, onRemove }: {
  skill: SkillDraft;
  onChange: (s: SkillDraft) => void;
  onDone: () => void;
  onRemove: () => void;
}) {
  return (
    <div className="flex flex-col gap-1.5 border border-base-content/10 rounded-md p-2">
      <div className="flex gap-1.5">
        <input
          value={skill.name}
          onChange={(e) => onChange({ ...skill, name: e.target.value })}
          placeholder="skill-name"
          className="input input-sm input-bordered w-40 font-mono"
        />
        <input
          value={skill.description}
          onChange={(e) => onChange({ ...skill, description: e.target.value })}
          placeholder="When should the agent use it? (one line)"
          className="input input-sm input-bordered flex-1 min-w-0"
        />
      </div>
      <textarea
        value={skill.body}
        onChange={(e) => onChange({ ...skill, body: e.target.value })}
        rows={4}
        placeholder="What the agent does when it uses this skill"
        className="textarea textarea-bordered textarea-sm w-full"
      />
      <div className="flex gap-2 justify-end">
        <button type="button" className="btn btn-xs btn-ghost" onClick={onRemove}>
          remove
        </button>
        <button type="button" className="btn btn-xs" onClick={onDone} disabled={!skill.name.trim()}>
          done
        </button>
      </div>
    </div>
  );
}

function PackEditor({ pack, targets, onBack, onSaved, onRequestDelete }: {
  pack: Pack | null; // null = a new pack
  targets: Targets;
  onBack: () => void;
  onSaved: (name: string) => void;
  onRequestDelete: (pack: Pack) => void;
}) {
  const initialSkills: SkillDraft[] = (pack?.skills ?? []).map((s) => ({ name: s.name, description: s.description, body: s.body }));
  const initialUses: Use[] = (pack?.attachments ?? []).map((a) => ({ kind: a.kind, target: a.target }));
  const [name, setName] = useState(pack?.name ?? "");
  const [text, setText] = useState(pack?.text ?? "");
  const [skills, setSkills] = useState<SkillDraft[]>(initialSkills);
  const [editing, setEditing] = useState<number | null>(null);
  const [uses, setUses] = useState<Use[]>(initialUses);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const contentChanged = text !== (pack?.text ?? "") || !sameJSON(skills, initialSkills);
  const usesChanged = !sameJSON(uses.map(keyOf).sort(), initialUses.map(keyOf).sort());
  const canSave = !saving && name.trim() !== "" && (pack === null ? text.trim() !== "" || skills.length > 0 : contentChanged || usesChanged);
  const used = new Set(uses.map(keyOf));

  async function save() {
    setSaving(true);
    setError(null);
    const packName = name.trim();
    try {
      if (pack === null) await client.createPack({ name: packName });
      if (pack === null || contentChanged) await client.savePackVersion({ name: packName, text, skills });
      const before = new Set(initialUses.map(keyOf));
      for (const u of uses) if (!before.has(keyOf(u))) await client.attachPack({ name: packName, attachment: u });
      for (const u of initialUses) if (!used.has(keyOf(u))) await client.detachPack({ name: packName, attachment: u as PackAttachment });
      onSaved(packName);
    } catch (err) {
      setError(errText(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-2">
        <button type="button" className="btn btn-xs btn-ghost" onClick={onBack}>
          ‹ back
        </button>
        {pack ? (
          <h3 className="font-semibold text-base flex-1">{pack.name}</h3>
        ) : (
          <input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value.toLowerCase())}
            placeholder="pack-name (lowercase, dashes)"
            className="input input-sm input-bordered flex-1 font-mono"
          />
        )}
        {pack && pack.latestVersion > 0 && <span className="badge badge-sm badge-ghost">v{pack.latestVersion}</span>}
      </div>

      <section className="flex flex-col gap-1.5">
        <h4 className="text-sm font-medium">1. Instructions the agent always follows</h4>
        <textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          rows={6}
          placeholder="e.g. Write short notes. One idea per note. Never commit without a link."
          className="textarea textarea-bordered textarea-sm w-full"
        />
      </section>

      <section className="flex flex-col gap-1.5">
        <div className="flex items-center">
          <h4 className="text-sm font-medium flex-1">
            2. Skills <span className="text-dim font-normal">(optional, used when relevant)</span>
          </h4>
          <button
            type="button"
            className="btn btn-xs"
            onClick={() => {
              setSkills((l) => [...l, { name: "", description: "", body: "" }]);
              setEditing(skills.length);
            }}
          >
            + add skill
          </button>
        </div>
        {skills.length === 0 && <p className="text-xs text-dim">No skills.</p>}
        {skills.map((s, i) =>
          editing === i ? (
            <SkillEditor
              key={i}
              skill={s}
              onChange={(next) => setSkills((l) => l.map((x, j) => (j === i ? next : x)))}
              onDone={() => setEditing(null)}
              onRemove={() => {
                setSkills((l) => l.filter((_, j) => j !== i));
                setEditing(null);
              }}
            />
          ) : (
            <div key={i} className="flex items-center gap-2 text-sm">
              <span className="font-mono">{s.name}</span>
              <span className="text-dim flex-1 truncate">— {s.description || "no description"}</span>
              <button type="button" className="btn btn-xs btn-ghost" aria-label={`edit skill ${s.name}`} onClick={() => setEditing(i)}>
                ✎
              </button>
              <button
                type="button"
                className="btn btn-xs btn-ghost"
                aria-label={`remove skill ${s.name}`}
                onClick={() => setSkills((l) => l.filter((_, j) => j !== i))}
              >
                ✕
              </button>
            </div>
          ),
        )}
      </section>

      <section className="flex flex-col gap-1.5">
        <h4 className="text-sm font-medium">3. Used by</h4>
        <div className="flex flex-wrap items-center gap-1.5">
          {uses.map((u) => (
            <span key={keyOf(u)} className="badge gap-1">
              {u.target}
              <span className="text-dim">{u.kind === "repo" ? "repo" : "agent"}</span>
              <button
                type="button"
                aria-label={`stop using in ${u.target}`}
                className="hover:text-error"
                onClick={() => setUses((l) => l.filter((x) => keyOf(x) !== keyOf(u)))}
              >
                ✕
              </button>
            </span>
          ))}
          <select
            value=""
            onChange={(e) => {
              const [kind, target] = e.target.value.split("/");
              if (target) setUses((l) => [...l, { kind, target }]);
            }}
            className="select select-sm select-bordered"
          >
            <option value="">+ add agent or repo</option>
            <optgroup label="Agents">
              {targets.definition.filter((t) => !used.has(`definition/${t}`)).map((t) => (
                <option key={t} value={`definition/${t}`}>
                  {t}
                </option>
              ))}
            </optgroup>
            <optgroup label="Repos">
              {targets.repo.filter((t) => !used.has(`repo/${t}`)).map((t) => (
                <option key={t} value={`repo/${t}`}>
                  {t}
                </option>
              ))}
            </optgroup>
          </select>
        </div>
        {uses.length === 0 && <p className="text-xs text-dim">Not used anywhere yet: saving keeps it, but no agent loads it.</p>}
      </section>

      <InlineError message={error} />

      <div className="flex items-center gap-3 pt-2 border-t border-base-content/10">
        {pack && (
          <button type="button" className="link link-error text-xs" onClick={() => onRequestDelete(pack)}>
            delete pack
          </button>
        )}
        <span className="flex-1" />
        <button type="button" className="btn btn-sm btn-primary" disabled={!canSave} onClick={save}>
          {saving ? "Saving…" : "Save — applies at next start"}
        </button>
      </div>
    </div>
  );
}

export function PacksPanel() {
  const [packs, setPacks] = useState<Pack[]>([]);
  const [targets, setTargets] = useState<Targets>({ definition: [], repo: [] });
  const [error, setError] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);
  // undefined = the list; null = a new pack; a name = that pack's editor.
  const [open, setOpen] = useState<string | null | undefined>(undefined);
  const [pendingDelete, setPendingDelete] = useState<Pack | null>(null);

  function load() {
    return Promise.all([client.listPacks({}), client.listDefinitions({}), client.listRepos({})])
      .then(([p, d, r]) => {
        setPacks(p.packs);
        setTargets({ definition: d.definitions.map((x) => x.name), repo: r.repos.map((x) => x.name) });
      })
      .catch((err: Error) => setError(err.message))
      .finally(() => setLoaded(true));
  }

  useEffect(() => {
    load();
  }, []);

  async function confirmDelete() {
    const pack = pendingDelete;
    setPendingDelete(null);
    if (!pack) return;
    try {
      await client.deletePack({ name: pack.name });
      setOpen(undefined);
      load();
    } catch (err) {
      setError(errText(err));
    }
  }

  const current = typeof open === "string" ? packs.find((p) => p.name === open) : undefined;

  return (
    <>
      {open === undefined ? (
        <div className="flex flex-col">
          <div className="flex items-center mb-1">
            <h3 className="font-semibold text-base flex-1">Packs</h3>
            <button type="button" className="btn btn-sm btn-primary" onClick={() => setOpen(null)}>
              + New pack
            </button>
          </div>
          <p className="text-xs text-dim mb-3">
            Instructions and skills an agent loads every time it starts. Fleet-wide rules change by pull request.
          </p>
          {packs.map((p) => (
            <button
              key={p.name}
              type="button"
              onClick={() => setOpen(p.name)}
              className="flex items-center gap-3 py-2.5 px-1 border-b border-base-content/5 last:border-0 text-left hover:bg-base-content/5"
            >
              <span className="font-medium font-mono">{p.name}</span>
              <span className="text-xs text-dim w-8">{p.latestVersion ? `v${p.latestVersion}` : "—"}</span>
              <span className="text-xs text-dim flex-1 truncate">
                {p.attachments.length ? `used by: ${p.attachments.map((a) => a.target).join(", ")}` : "not used yet"}
              </span>
              <span className="text-dim">›</span>
            </button>
          ))}
          {packs.length === 0 && <p className="text-sm text-dim py-2">{loaded ? "No packs yet." : "Loading…"}</p>}
          <InlineError message={error} />
        </div>
      ) : (
        <PackEditor
          key={open ?? "new"}
          pack={current ?? null}
          targets={targets}
          onBack={() => setOpen(undefined)}
          onSaved={(name) => {
            load();
            setOpen(name);
          }}
          onRequestDelete={setPendingDelete}
        />
      )}

      <ConfirmModal
        open={pendingDelete !== null}
        message={
          pendingDelete
            ? `Delete pack "${pendingDelete.name}" and stop using it in ${pendingDelete.attachments.length} place(s)? Running sessions keep it until their next start.`
            : ""
        }
        confirmWord="delete"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </>
  );
}
