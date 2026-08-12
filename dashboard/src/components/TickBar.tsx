import type { TodoItem } from "../transcript";

// The mockups' todo progress: one discrete segment per todo rather than a
// single percentage bar, so "3 of 6, and the current one is what you're
// blocking" is readable at a glance without a label.
//
// The in-progress segment is pink when the session is blocked on a human and
// amber when it's just working — that difference is the whole point of the
// control appearing on a NEEDS YOU card and on a WORKING row.

export function TickBar({
  todos,
  blocked = false,
  cell = "flex",
  className = "",
}: {
  todos: TodoItem[];
  blocked?: boolean;
  // fixed widths match the mockups: 16px desktop cards, 11px mobile cards,
  // `flex` for the full-width bars in the panels and the mobile detail header.
  cell?: "flex" | "w-4" | "w-[11px]";
  className?: string;
}) {
  if (todos.length === 0) return null;
  const width = cell === "flex" ? "flex-1" : cell;
  return (
    <div className={`flex ${cell === "flex" ? "gap-0.5" : "gap-[3px]"} ${className}`}>
      {todos.map((t, i) => (
        <div
          key={i}
          className={`${width} h-[3px] ${
            t.status === "completed"
              ? "bg-success"
              : t.status === "in_progress"
                ? blocked
                  ? "bg-error"
                  : "bg-info"
                : "bg-line"
          }`}
        />
      ))}
    </div>
  );
}

export function todoProgress(todos: TodoItem[]): string {
  return `${todos.filter((t) => t.status === "completed").length}/${todos.length}`;
}
