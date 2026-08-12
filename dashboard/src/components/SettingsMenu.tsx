import { ManageReposModal } from "./ManageReposModal";
import { ManagePromptSnippetsModal } from "./ManagePromptSnippetsModal";
import { Segmented } from "./Segmented";
import type { Theme } from "../useTheme";

const THEMES: readonly { value: Theme; label: string }[] = [
  { value: "herd", label: "dark" },
  { value: "herd-light", label: "light" },
];

// Neither console mockup gives the repo/prompt-snippet editors a home — the
// header they show is full, and audits got promoted to its own view. Rather
// than drop two working features, both triggers live behind one ⚙, together
// with the theme switch (which the mockups show as canvas chrome outside the
// device frame, so it needs a real home too).
//
// A <details> dropdown, not a Modal: both editors open their own <dialog>, and
// a <dialog> nested inside an open one closes both on cancel — a trap
// ManageReposModal's own comments record learning the hard way.
export function SettingsMenu({
  theme,
  onThemeChange,
}: {
  theme: Theme;
  onThemeChange: (t: Theme) => void;
}) {
  return (
    <details className="dropdown dropdown-end flex-none">
      <summary
        aria-label="Settings"
        className="list-none cursor-pointer px-1.5 py-1 text-[13px] text-dim hover:text-base-content"
      >
        ⚙
      </summary>
      <div className="dropdown-content z-10 mt-1 w-56 border border-line bg-base-200 p-3 flex flex-col gap-3 shadow-lg">
        <div className="flex flex-col gap-1.5">
          <span className="text-[10px] tracking-[0.12em] text-dim2">THEME</span>
          <Segmented value={theme} options={THEMES} onChange={onThemeChange} grow size="sm" />
        </div>
        <div className="flex flex-col gap-1.5">
          <span className="text-[10px] tracking-[0.12em] text-dim2">MANAGE</span>
          <ManageReposModal />
          <ManagePromptSnippetsModal />
        </div>
      </div>
    </details>
  );
}
