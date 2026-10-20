import { useEffect, useState } from "react";
import { client } from "../connectClient";
import { ConfirmModal } from "./ConfirmModal";
import type { Pack } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "./InlineError";

// Live context packs (docs/adr/0063): instructions and skills that load into
// every start of the agents and repos that use them. Never sent as a
// message. A list, then one focused editor per pack: everything in the editor
// is staged and applied by its one Save (a new version if the text or skills
// changed, then the "used by" changes). Fleet-wide packs (packs/fleet,
// packs/repo) change by pull request and are not shown here.

type Targets = { definition: string[]; repo: string[] };
type SkillDraft = { id: number; name: string; description: string; body: string };
type Use = { kind: string; target: string };

const keyOf = (u: Use) => `${u.kind}/${u.target}`;
const errText = (err: unknown) => (err instanceof Error ? err.message : String(err));
// What the server stores, without the editor's own ids.
const contentOf = (text: string, skills: SkillDraft[]) =>
  JSON.stringify({ text, skills: skills.map(({ name, description, body }) => ({ name, description, body })) });

let nextSkillId = 1;

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
          aria-label="skill name"
          value={skill.name}
          onChange={(e) => onChange({ ...skill, name: e.target.value })}
          placeholder="skill-name"
          className="input input-sm input-bordered w-40 font-mono"
        />
        <input
          aria-label="when to use the skill"
          value={skill.description}
          onChange={(e) => onChange({ ...skill, description: e.target.value })}
          placeholder="When should the agent use it? (one line)"
          className="input input-sm input-bordered flex-1 min-w-0"
        />
      </div>
      <textarea
        aria-label="skill instructions"
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
  onSaved: (name: string) => Promise<void>;
  onRequestDelete: (pack: Pack) => void;
}) {
  const [initialSkills] = useState<SkillDraft[]>(() =>
    (pack?.skills ?? []).map((s) => ({ id: nextSkillId++, name: s.name, description: s.description, body: s.body })),
  );
  const initialUses: Use[] = (pack?.attachments ?? []).map((a) => ({ kind: a.kind, target: a.target }));
  const [name, setName] = useState(pack?.name ?? "");
  const [text, setText] = useState(pack?.text ?? "");
  const [skills, setSkills] = useState<SkillDraft[]>(initialSkills);
  const [editing, setEditing] = useState<number | null>(null);
  const [uses, setUses] = useState<Use[]>(initialUses);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // What the server already has. A save is several calls; when one fails,
  // the retry resumes from here instead of re-creating the pack or re-saving
  // a version that went through.
  const [done, setDone] = useState(() => ({
    created: pack !== null,
    content: contentOf(pack?.text ?? "", initialSkills),
    uses: new Set(initialUses.map(keyOf)),
  }));

  const used = new Set(uses.map(keyOf));
  const contentChanged = contentOf(text, skills) !== done.content;
  const usesChanged = used.size !== done.uses.size || [...used].some((k) => !done.uses.has(k));
  const skillsNamed = skills.every((s) => s.name.trim() !== "");
  const canSave =
    !saving &&
    name.trim() !== "" &&
    skillsNamed &&
    (!done.created ? text.trim() !== "" || skills.length > 0 : contentChanged || usesChanged);

  async function save() {
    setSaving(true);
    setError(null);
    const packName = name.trim();
    // A working copy; whatever went through is kept even when a later call fails.
    const d = { ...done, uses: new Set(done.uses) };
    try {
      if (!d.created) {
        await client.createPack({ name: packName });
        d.created = true;
      }
      const content = contentOf(text, skills);
      if (content !== d.content) {
        await client.savePackVersion({
          name: packName,
          text,
          skills: skills.map(({ name, description, body }) => ({ name, description, body })),
        });
        d.content = content;
      }
      for (const u of uses) {
        if (d.uses.has(keyOf(u))) continue;
        await client.attachPack({ name: packName, attachment: u });
        d.uses.add(keyOf(u));
      }
      for (const k of [...d.uses]) {
        if (used.has(k)) continue;
        const [kind, ...rest] = k.split("/");
        await client.detachPack({ name: packName, attachment: { kind, target: rest.join("/") } });
        d.uses.delete(k);
      }
      // Stays "saving" until the list has reloaded: the editor then remounts
      // on the fresh server state, so nothing can be saved twice.
      await onSaved(packName);
    } catch (err) {
      setDone(d);
      setError(errText(err));
      setSaving(false);
    }
  }

  function removeSkill(id: number) {
    setSkills((l) => l.filter((s) => s.id !== id));
    if (editing === id) setEditing(null);
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
            aria-label="pack name"
            value={name}
            disabled={done.created}
            onChange={(e) => setName(e.target.value.toLowerCase())}
            placeholder="pack-name (lowercase, dashes)"
            className="input input-sm input-bordered flex-1 font-mono"
          />
        )}
        {pack && pack.latestVersion > 0 && <span className="badge badge-sm badge-ghost">v{pack.latestVersion}</span>}
      </div>

      <section className="flex flex-col gap-1.5">
        <h4 className="text-sm font-medium" id="pack-instructions">
          1. Instructions the agent always follows
        </h4>
        <textarea
          aria-labelledby="pack-instructions"
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
              const id = nextSkillId++;
              setSkills((l) => [...l, { id, name: "", description: "", body: "" }]);
              setEditing(id);
            }}
          >
            + add skill
          </button>
        </div>
        {skills.length === 0 && <p className="text-xs text-dim">No skills.</p>}
        {skills.map((s) =>
          editing === s.id ? (
            <SkillEditor
              key={s.id}
              skill={s}
              onChange={(next) => setSkills((l) => l.map((x) => (x.id === s.id ? next : x)))}
              onDone={() => setEditing(null)}
              onRemove={() => removeSkill(s.id)}
            />
          ) : (
            <div key={s.id} className="flex items-center gap-2 text-sm">
              <span className="font-mono">{s.name || "(unnamed)"}</span>
              <span className="text-dim flex-1 truncate">— {s.description || "no description"}</span>
              <button type="button" className="btn btn-xs btn-ghost" aria-label={`edit skill ${s.name}`} onClick={() => setEditing(s.id)}>
                ✎
              </button>
              <button type="button" className="btn btn-xs btn-ghost" aria-label={`remove skill ${s.name}`} onClick={() => removeSkill(s.id)}>
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
            aria-label="add an agent or repo"
            value=""
            onChange={(e) => {
              const [kind, ...rest] = e.target.value.split("/");
              if (rest.length) setUses((l) => [...l, { kind, target: rest.join("/") }]);
            }}
            className="select select-sm select-bordered"
          >
            <option value="">+ add agent or repo</option>
            <optgroup label="Agents">
              {targets.definition
                .filter((t) => !used.has(`definition/${t}`))
                .map((t) => (
                  <option key={t} value={`definition/${t}`}>
                    {t}
                  </option>
                ))}
            </optgroup>
            <optgroup label="Repos">
              {targets.repo
                .filter((t) => !used.has(`repo/${t}`))
                .map((t) => (
                  <option key={t} value={`repo/${t}`}>
                    {t}
                  </option>
                ))}
            </optgroup>
          </select>
        </div>
        {uses.length === 0 && <p className="text-xs text-dim">Not used anywhere yet: saving keeps it, but no agent loads it.</p>}
      </section>

      {!skillsNamed && <p className="text-xs text-warning">Every skill needs a name before saving.</p>}
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
    // Back to the list either way: that is where the error is shown.
    setOpen(undefined);
    try {
      await client.deletePack({ name: pack.name });
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
          // Remounts on the server's state after every save.
          key={current ? `${current.name}@${current.latestVersion}/${current.attachments.map((a) => a.kind + a.target).join(",")}` : "new"}
          pack={current ?? null}
          targets={targets}
          onBack={() => setOpen(undefined)}
          onSaved={async (name) => {
            await load();
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
