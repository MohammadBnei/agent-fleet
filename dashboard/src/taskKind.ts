import type { Task } from "./gen/agentfleet/v1/core_pb";

// docs/adr/0037: a thot session is an ordinary worker task on
// infra-bootstrap, so `kind` is the only thing distinguishing it. These
// helpers live here (not inline) because desktop and mobile each render
// the same information from their own components — a branch inlined in
// one is a branch missing from the other.

export function isThot(task: { kind?: string } | undefined | null): boolean {
  return task?.kind === "thot";
}

// What to show where a task's repo normally goes. A thot session's repo is
// literally infra-bootstrap, but showing that is misleading — the session
// is about the cluster, and only incidentally about that repo's files.
export function repoLabel(task: { kind?: string; repo?: string } | undefined | null): string {
  if (!task) return "";
  return isThot(task) ? "cluster" : (task.repo ?? "");
}
