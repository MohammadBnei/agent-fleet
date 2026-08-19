import type { CSSProperties } from "react";

// A checkbox dropdown for one filter axis, used for both status and repo.
//
// It replaces, on the status axis, two overlapping controls (chips plus a
// separate "hide terminal" toggle that said the same thing more coarsely) and,
// on the repo axis, a single-choice <select> that could only ever narrow to one
// repo — there was no way to say "these two but not that one".
//
// Native Popover API + CSS anchor positioning, the pattern ActionsMenu and
// SettingsMenu already use: no z-index, no click-outside handler, Esc-to-close
// for free.
//
// `id` is required and must be unique on the page. The other popovers in this
// codebase hardcode theirs and explain that only one instance is ever mounted;
// that stops being true here the moment a second axis appears in the same row,
// and two elements sharing a popovertarget silently drives the wrong one.
//
// The set is what to HIDE, not what to show. Seeding "shown" would need every
// option known up front, and the repo list only exists once sessions have
// loaded — so an empty set is the honest "nothing filtered" starting state for
// both axes.
export function MultiSelectFilter({
  id,
  noun,
  options,
  hidden,
  onToggle,
}: {
  id: string;
  // Plural, lowercase — "repos", "status". Read straight into the label.
  noun: string;
  options: string[];
  hidden: Set<string>;
  onToggle: (value: string) => void;
}) {
  const shown = options.filter((o) => !hidden.has(o)).length;
  const filtered = shown !== options.length;
  const anchor = `--anchor-${id}`;
  return (
    <>
      <button
        type="button"
        popoverTarget={id}
        style={{ anchorName: anchor } as CSSProperties}
        aria-label={`filter by ${noun}`}
        // whitespace-nowrap and flex-none: this label must never be the thing
        // that wraps the control row.
        className={`border bg-transparent text-xs px-1.5 py-1 cursor-pointer whitespace-nowrap flex-none hover:text-base-content ${
          filtered ? "border-primary text-primary" : "border-line text-dim"
        }`}
      >
        {/* "2/3" rather than "hiding 1": the count you care about is what you
            are looking at, and it states the filter instead of its negation. */}
        {filtered ? `${noun} ${shown}/${options.length}` : `all ${noun}`} ▾
      </button>
      <ul
        className="dropdown menu menu-sm bg-base-100 rounded-box shadow w-52 p-1 max-h-72 overflow-y-auto"
        popover="auto"
        id={id}
        style={{ positionAnchor: anchor } as CSSProperties}
      >
        {/* Clearing a filter was N clicks, one per option, with the list
            re-sorting under you as you went. This is the one control that says
            "stop filtering" — and, when everything is already shown, "show me
            only one thing" in two clicks instead of N-1.

            No onSetAll prop: onToggle is a functional state update in both
            callers, so flipping each option that disagrees with the target
            composes correctly and keeps this component's surface at one
            callback. */}
        <li>
          <label className="flex items-center gap-2 cursor-pointer font-medium">
            <input
              type="checkbox"
              checked={hidden.size === 0}
              // The third state. Without it a partial selection renders an
              // unchecked box, which reads as "nothing is selected" when in
              // fact most things are. It is a DOM property, not an attribute,
              // so it can only be set through a ref.
              ref={(el) => {
                if (el) el.indeterminate = hidden.size > 0 && shown > 0;
              }}
              onChange={() => {
                const hideEverything = hidden.size === 0;
                for (const o of options) {
                  if (hideEverything !== hidden.has(o)) onToggle(o);
                }
              }}
              className="checkbox checkbox-xs flex-none"
            />
            all {noun}
          </label>
        </li>
        <li className="my-1 h-px bg-line2 pointer-events-none" />
        {options.map((o) => (
          <li key={o}>
            {/* The label wraps the checkbox so the whole row is the target, not
                a 14px box. Deliberately NOT popoverTargetAction="hide": this is
                a multi-select, so picking one has to leave it open. */}
            <label className="flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={!hidden.has(o)}
                onChange={() => onToggle(o)}
                className="checkbox checkbox-xs flex-none"
              />
              <span className="min-w-0 truncate">{o}</span>
            </label>
          </li>
        ))}
      </ul>
    </>
  );
}
