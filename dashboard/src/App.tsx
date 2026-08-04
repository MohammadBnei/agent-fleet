import { useState } from "react";
import { TaskList } from "./pages/TaskList";
import { TaskDetail } from "./pages/TaskDetail";

// No router library for two views (see docs/adr/0013's plan) — state
// mirrored to ?task=<id> so a task's detail view is still bookmarkable/
// shareable without pulling in react-router for this little surface.
function readTaskIdFromUrl(): string | null {
  return new URLSearchParams(window.location.search).get("task");
}

export default function App() {
  const [selectedId, setSelectedId] = useState<string | null>(
    readTaskIdFromUrl,
  );

  function selectTask(id: string) {
    setSelectedId(id);
    const url = new URL(window.location.href);
    url.searchParams.set("task", id);
    window.history.pushState({}, "", url);
  }

  return (
    <div className="min-h-screen bg-base-100">
      <div className="navbar bg-base-200">
        <span className="text-lg font-semibold px-2">agent-fleet</span>
      </div>
      <div className="flex flex-col lg:flex-row">
        <div className="lg:w-1/2 border-b lg:border-b-0 lg:border-r border-base-300">
          <TaskList selectedId={selectedId} onSelect={selectTask} />
        </div>
        <div className="lg:w-1/2">
          {selectedId ? (
            <TaskDetail taskId={selectedId} />
          ) : (
            <div className="p-4 opacity-60">Select a task to view details.</div>
          )}
        </div>
      </div>
    </div>
  );
}
