import { useEffect, useState } from "react";
import { client } from "../connectClient";
import type { DefinitionGrant } from "../gen/agentfleet/v1/dashboard_pb";
import { InlineError } from "./InlineError";

// Live definition grants (docs/adr/0076 §3): rules an admin remembered for a
// whole definition from a permission card. They are unreviewed state, so they
// are listed here to be revoked or promoted into the definition's
// tools.allow by a PR. A revoke reaches running sessions at their next warm.
export function GrantsPanel() {
  const [grants, setGrants] = useState<DefinitionGrant[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<bigint | null>(null);

  const load = () =>
    client.listGrants({}).then(
      (r) => setGrants(r.grants),
      (e: unknown) => setError(String(e)),
    );
  useEffect(() => {
    void load();
  }, []);

  const revoke = async (g: DefinitionGrant) => {
    setRevoking(g.id);
    setError(null);
    try {
      await client.revokeGrant({ id: g.id });
      await load();
    } catch (e) {
      setError(String(e));
    } finally {
      setRevoking(null);
    }
  };

  return (
    <div>
      <p className="text-xs text-dim mb-3">
        Approvals remembered for a whole definition. Running sessions keep a revoked rule until they next warm. To keep one, add it to the definition's tools.allow and revoke it here.
      </p>
      {error && <InlineError message={error} />}
      {grants === null ? (
        <div className="text-xs text-dim">loading…</div>
      ) : grants.length === 0 ? (
        <div className="text-xs text-dim">No definition grants.</div>
      ) : (
        grants.map((g) => (
          <div key={String(g.id)} className="flex items-center gap-2 flex-wrap py-1.5 border-b border-base-content/5 last:border-0">
            <span className="text-sm font-medium w-36 truncate flex-none">{g.definition}</span>
            <code className="text-xs flex-1 min-w-0 break-all">{g.rule}</code>
            <span className="text-xs text-dim flex-none" title={g.grantedAt}>
              {g.grantedBy}
            </span>
            <button
              type="button"
              disabled={revoking !== null}
              onClick={() => void revoke(g)}
              className="border border-line px-3 py-1 text-xs hover:border-error hover:text-error disabled:opacity-50 cursor-pointer"
            >
              {revoking === g.id ? "revoking…" : "revoke"}
            </button>
          </div>
        ))
      )}
    </div>
  );
}
