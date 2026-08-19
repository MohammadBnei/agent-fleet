import { MultiSelectFilter } from "./MultiSelectFilter";
import type { SortKey } from "../pages/SessionList";

// The quiet tail's controls, shared by both list views. They were two
// hand-written rows: desktop's ControlBar and a mobile copy that had drifted to
// different option labels and a different filter model. The axes are identical
// and the drift was accidental, so there is one row now.
//
// `compact` is the only difference the phone actually needs: the selects share
// the slack instead of sizing to content, and nothing wraps.
export function QuietControls({
  sort,
  setSort,
  repos,
  hiddenRepos,
  toggleRepo,
  statuses,
  hiddenStatuses,
  toggleStatus,
  compact,
}: {
  sort: SortKey;
  setSort: (s: SortKey) => void;
  repos: string[];
  hiddenRepos: Set<string>;
  toggleRepo: (r: string) => void;
  statuses: string[];
  hiddenStatuses: Set<string>;
  toggleStatus: (s: string) => void;
  compact?: boolean;
}) {
  const select = `border border-line bg-transparent text-xs text-dim px-1.5 py-1 cursor-pointer ${
    compact ? "min-w-0 flex-1" : ""
  }`;
  return (
    // No flex-wrap when compact: one row is a requirement on a phone, not a
    // preference, and min-w-0 on the selects is what lets them shrink to hold it.
    <div className={`flex items-center ${compact ? "gap-1.5 min-w-0" : "gap-2 flex-wrap"}`}>
      <select
        className={select}
        value={sort}
        onChange={(e) => setSort(e.target.value as SortKey)}
        aria-label="sort sessions"
      >
        <option value="date">sort: recent</option>
        <option value="status">sort: status</option>
        <option value="repo">sort: repo</option>
        <option value="title">sort: title</option>
      </select>
      <MultiSelectFilter id="filter-repo" noun="repos" options={repos} hidden={hiddenRepos} onToggle={toggleRepo} />
      <MultiSelectFilter
        id="filter-status"
        noun="status"
        options={statuses}
        hidden={hiddenStatuses}
        onToggle={toggleStatus}
      />
    </div>
  );
}
