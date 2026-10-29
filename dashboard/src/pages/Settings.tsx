import { useState } from "react";
import { GrantsPanel } from "../components/GrantsPanel";
import { PacksPanel } from "../components/PacksPanel";
import { ReposPanel } from "../components/ReposPanel";
import { Segmented } from "../components/Segmented";
import type { FeedWidth } from "../useFeedWidth";
import { useIdentity } from "../useIdentity";
import type { Theme } from "../useTheme";

// The ⚙ page. It replaced a popover that held three modal triggers, a theme
// switch and the sign-out link: a third editor (context packs) made a menu of
// modals the wrong shape. Each section is the full editor, inline.

type Section = "packs" | "grants" | "repos" | "appearance" | "account";

const SECTIONS: readonly { value: Section; label: string; hint: string }[] = [
  { value: "packs", label: "Packs", hint: "What agents load at start" },
  {
    value: "grants",
    label: "Grants",
    hint: "Approvals remembered for a definition",
  },
  { value: "repos", label: "Repos", hint: "Repos in the fleet" },
  { value: "appearance", label: "Appearance", hint: "Theme and text width" },
  { value: "account", label: "Account", hint: "Who is signed in" },
];

const THEMES: readonly { value: Theme; label: string }[] = [
  { value: "herd", label: "dark" },
  { value: "herd-light", label: "light" },
];

// The feed's prose measure. 760 is the readability cap the console shipped
// with; it also squeezed code blocks and diagrams that have no such limit, so
// it's a choice now rather than a number. See useFeedWidth.
const WIDTHS: readonly { value: FeedWidth; label: string }[] = [
  { value: "760", label: "760" },
  { value: "1100", label: "1100" },
  { value: "full", label: "full" },
];

function readSection(): Section {
  const s = new URLSearchParams(window.location.search).get("section");
  return SECTIONS.some((x) => x.value === s) ? (s as Section) : "packs";
}

export function Settings({
  theme,
  onThemeChange,
  feedWidth,
  onFeedWidthChange,
}: {
  theme: Theme;
  onThemeChange: (t: Theme) => void;
  feedWidth: FeedWidth;
  onFeedWidthChange: (w: FeedWidth) => void;
}) {
  const [section, setSection] = useState<Section>(readSection);
  const identity = useIdentity();

  function choose(next: Section) {
    setSection(next);
    // replaceState: switching sections is not navigation worth a Back press.
    const url = new URL(window.location.href);
    url.searchParams.set("section", next);
    window.history.replaceState({}, "", url);
  }

  return (
    <div className="flex-1 min-h-0 overflow-y-auto">
      <div className="mx-auto max-w-4xl flex flex-col md:flex-row gap-6 px-4 py-5">
        <nav className="md:w-48 flex-none">
          <h2 className="text-2xs tracking-[0.12em] text-dim2 mb-2">
            SETTINGS
          </h2>
          {/* Mobile: one picker. Desktop: the list. */}
          <select
            value={section}
            onChange={(e) => choose(e.target.value as Section)}
            className="select select-sm select-bordered w-full md:hidden"
          >
            {SECTIONS.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
          <ul className="hidden md:flex flex-col gap-0.5">
            {SECTIONS.map((s) => (
              <li key={s.value}>
                <button
                  type="button"
                  onClick={() => choose(s.value)}
                  aria-current={section === s.value ? "page" : undefined}
                  className={`w-full text-left px-2 py-1.5 border-l-2 ${
                    section === s.value
                      ? "border-primary bg-base-content/5 text-base-content"
                      : "border-transparent text-dim hover:text-base-content"
                  }`}
                >
                  <div className="text-sm">{s.label}</div>
                  <div className="text-2xs text-dim2">{s.hint}</div>
                </button>
              </li>
            ))}
          </ul>
        </nav>

        <main className="flex-1 min-w-0">
          {section === "packs" && <PacksPanel />}
          {section === "grants" && <GrantsPanel />}
          {section === "repos" && <ReposPanel />}
          {section === "appearance" && (
            <div className="flex flex-col gap-4 max-w-sm">
              <h3 className="font-semibold text-base">Appearance</h3>
              <div className="flex flex-col gap-1.5">
                <span className="text-2xs tracking-[0.12em] text-dim2">
                  THEME
                </span>
                <Segmented
                  value={theme}
                  options={THEMES}
                  onChange={onThemeChange}
                  grow
                  size="sm"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <span className="text-2xs tracking-[0.12em] text-dim2">
                  TEXT WIDTH
                </span>
                <Segmented
                  value={feedWidth}
                  options={WIDTHS}
                  onChange={onFeedWidthChange}
                  grow
                  size="sm"
                />
              </div>
            </div>
          )}
          {section === "account" && (
            <div className="flex flex-col gap-2">
              <h3 className="font-semibold text-base">Account</h3>
              {/*
                Absent when the gate is off (FLEET_AUTH_DISABLED=1): no identity
                is the honest display for a local stack that has none. A plain
                link for sign-out: /auth/logout clears the cookie and redirects,
                and a full page load drops the SPA state of the ended session.
              */}
              {identity?.email ? (
                <>
                  <span className="text-sm">
                    Signed in as{" "}
                    <span className="font-medium">{identity.email}</span>
                  </span>
                  <a
                    href="/auth/logout"
                    className="text-xs text-dim underline underline-offset-2 hover:text-base-content self-start"
                  >
                    sign out
                  </a>
                </>
              ) : (
                <span className="text-sm text-dim">
                  Sign-in is off on this console (local stack).
                </span>
              )}
            </div>
          )}
        </main>
      </div>
    </div>
  );
}
