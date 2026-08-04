import { useEffect, useState } from "react";
import { listTasks, type Task } from "../api";

const STATUS_BADGE: Record<string, string> = {
  pending: "badge-neutral",
  claimed: "badge-info",
  planning: "badge-info",
  done: "badge-success",
  failed: "badge-error",
  cancelled: "badge-warning",
};

// Plain polling, not SSE — a second global live feed just for the list is
// unjustified scope for v1 (see docs/adr/0013); the detail view's SSE
// stream is where "live" actually matters.
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
      listTasks()
        .then((list) => {
          if (!cancelled) setTasks(list);
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
