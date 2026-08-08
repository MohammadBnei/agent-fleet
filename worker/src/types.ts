// Replaces db.ts's Task interface — massively simplified, since a
// single-shot worker pod (docs/adr/0019) no longer claims, retries, or
// tracks its own resume state: core's dispatch loop and the provisioner
// own the whole lifecycle up to the moment this pod exists. Everything the
// worker needs arrives via env at pod creation time (see
// provisioner/internal/k8s/pod.go's CreateWorkerPod), not a fetched row.
export type Task = {
  id: string;
  repo: string;
  description: string;
  leaseId: string;
  // baseBranch threads into the merged task prompt (reliability-findings.md
  // #0) so the agent's own `gh pr create --base` is correct — some repos
  // don't develop off "main" (e.g. vos-monolith uses "dev").
  baseBranch: string;
  // The operator's chosen prompt_snippets, already resolved and joined by
  // core at task-creation time (docs/adr/0028-style dashboard-editable
  // guidance, replacing the old unconditional hardcoded workflow prompt).
  // "" when no snippets were attached — the task's base prompt is then
  // just its own description, nothing more.
  guidance: string;
};
