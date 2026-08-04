import { useEffect, useState } from "react";
import { client } from "../connectClient";
import type { Task } from "../gen/agentfleet/v1/dashboard_pb";

const STATUS_BADGE: Record<string, string> = {
  pending: "badge-neutral",
  claimed: "badge-info",
  planning: "badge-info",
  done: "badge-success",
  failed: "badge-error",
  cancelled: "badge-warning",
};

// Plain polling, not a stream — a second live feed just for the list is
// unjustified scope for v1 (see docs/adr/0014); the detail view's
// StreamTranscript RPC is where "live" actually matters.
const POLL_INTERVAL_MS = 5000;

export function TaskList({
  selectedId,
  onSelect,
}: {
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = () => {
      client
        .listTasks({})
        .then((res) => {
          if (!cancelled) setTasks(res.tasks);
        })
        .catch((err: Error) => {
          if (!cancelled) setError(err.message);
        });
    };
    load();
    const interval = setInterval(load, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, []);

  if (error) return <div className="alert alert-error m-4">{error}</div>;

  return (
    <div className="overflow-x-auto">
      <table className="table table-zebra">
        <thead>
          <tr>
            <th>Repo</th>
            <th>Description</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {tasks.map((task) => (
            <tr
              key={task.id}
              className={`hover:bg-base-200 cursor-pointer ${
                task.id === selectedId ? "bg-base-200" : ""
              }`}
              onClick={() => onSelect(task.id)}
            >
              <td>{task.repo}</td>
              <td className="max-w-xs truncate">{task.description}</td>
              <td>
                <span
                  className={`badge ${STATUS_BADGE[task.status] ?? "badge-neutral"}`}
                >
                  {task.status}
                </span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
