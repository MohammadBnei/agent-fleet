import type { CSSProperties } from "react";
import { ManageReposModal } from "./ManageReposModal";
import { ManagePromptSnippetsModal } from "./ManagePromptSnippetsModal";
import { Segmented } from "./Segmented";
import type { Theme } from "../useTheme";
import { useIdentity } from "../useIdentity";

const THEMES: readonly { value: Theme; label: string }[] = [
  { value: "herd", label: "dark" },
  { value: "herd-light", label: "light" },
];

// Neither console mockup gives the repo/prompt-snippet editors a home — the
// header they show is full, and schedules got promoted to their own view. Rather
// than drop two working features, both triggers live behind one ⚙, together
// with the theme switch (which the mockups show as canvas chrome outside the
// device frame, so it needs a real home too).
//
// Not a Modal: both editors open their own <dialog>, and a <dialog> nested
// inside an open one closes both on cancel — a trap ManageReposModal's own
// comments record learning the hard way.
//
// Native Popover API + CSS anchor positioning, the same pattern ActionsMenu's
// mode dropdown uses. `popover="auto"` is what buys the click-outside close
// the old <details> never had (a <details> only closes by clicking its own
// summary), plus Esc-to-close and top-layer rendering, so no z-index and no
// clipping by the header's own overflow. One instance is mounted at a time,
// so a static id/anchor-name pair is safe.
//
// Light dismiss walks the DOM, not the screen: the editors' <dialog>s are
// descendants of the popover, so clicking inside one does NOT dismiss the
// panel out from under it (verified in Chrome — the failure mode would be a
// modal vanishing mid-edit, since a display:none ancestor takes its
// top-layer descendants with it).
//
// ponytail: the layout utilities live on an inner wrapper, NOT on the
// popover element. daisyUI hides a closed popover with
// `.dropdown[popover]:not(:popover-open){display:none}`, emitted inside
// `@layer utilities{@layer daisyui.l1.l2.l3{…}}`; Tailwind's `.flex` is
// emitted *unlayered* in `@layer utilities`, and unlayered declarations
// beat their own layer's sublayers regardless of specificity. A `flex` on
// the popover itself therefore wins, and the closed panel stays
// display:flex — invisible (opacity:0) but laid out and hit-testable, a
// w-56 box parked under the ⚙ eating taps meant for the buttons beside it.
// Any Tailwind *display* utility here re-breaks it; put them on the child.
export function SettingsMenu({
  theme,
  onThemeChange,
}: {
  theme: Theme;
  onThemeChange: (t: Theme) => void;
}) {
  const identity = useIdentity();

  return (
    <>
      <button
        type="button"
        aria-label="Settings"
        popoverTarget="popover-settings"
        style={{ anchorName: "--anchor-settings" } as CSSProperties}
        className="flex-none cursor-pointer px-1.5 py-1 text-base text-dim hover:text-base-content"
      >
        ⚙
      </button>
      <div
        id="popover-settings"
        popover="auto"
        style={{ positionAnchor: "--anchor-settings" } as CSSProperties}
        className="dropdown dropdown-end mt-1 w-56 border border-line bg-base-200 p-3 shadow-lg"
      >
        <div className="flex flex-col gap-3">
          <div className="flex flex-col gap-1.5">
            <span className="text-2xs tracking-[0.12em] text-dim2">THEME</span>
            <Segmented value={theme} options={THEMES} onChange={onThemeChange} grow size="sm" />
          </div>
          <div className="flex flex-col gap-1.5">
            <span className="text-2xs tracking-[0.12em] text-dim2">MANAGE</span>
            <ManageReposModal />
            <ManagePromptSnippetsModal />
          </div>
          {/*
            Nothing in the console said an identity was involved. With
            basic-admin-auth gone, authentik is the only gate, and its redirect
            is fast enough that the console looked exactly as it did when one
            shared password let anyone in — which is how you end up unsure
            whether SSO is actually on.

            Absent when the gate is off (FLEET_AUTH_DISABLED=1) rather than
            rendering an empty row: no identity is the honest display for a
            local stack that has none.
          */}
          {identity?.email && (
            <div className="flex flex-col gap-1.5 border-t border-line pt-3">
              <span className="text-2xs tracking-[0.12em] text-dim2">SIGNED IN</span>
              <span className="truncate text-xs text-base-content" title={identity.email}>
                {identity.email}
              </span>
              {/*
                A plain link, not a fetch: /auth/logout clears the cookie and
                redirects, and letting the browser follow it is the whole
                interaction. Full page load on purpose — the SPA's in-memory
                state belongs to the session being ended.
              */}
              <a
                href="/auth/logout"
                className="text-2xs text-dim underline underline-offset-2 hover:text-base-content"
              >
                sign out
              </a>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
