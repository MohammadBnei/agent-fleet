import { useEffect } from "react";
import { useLocalStorageState } from "./useLocalStorageState";

export type Theme = "herd" | "herd-light";

// Key is duplicated in index.html's pre-paint script — that script is what
// stops a light-theme user seeing the dark default flash on every load, so if
// this key changes, change it there too.
const THEME_KEY = "herd.theme";

// --panel, not --bg: theme-color paints the OS/browser chrome butted against the
// app's own top bar, which is bg-base-200. An installed PWA uses it for the
// status bar, so a light-theme install with a dark value gets a black strip.
const THEME_COLOR: Record<Theme, string> = {
  herd: "#14121a",
  "herd-light": "#f8f6f2",
};

export function useTheme() {
  const [theme, setTheme] = useLocalStorageState<Theme>(THEME_KEY, "herd");
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.querySelector('meta[name="theme-color"]')?.setAttribute("content", THEME_COLOR[theme]);
    // The two SVG favicon links resolve by `prefers-color-scheme`, but the app's
    // theme is an explicit choice that can disagree with the OS. Make the
    // matching one win outright rather than shipping a light mark on a dark tab.
    const light = theme === "herd-light";
    for (const link of document.querySelectorAll<HTMLLinkElement>('link[rel="icon"][type="image/svg+xml"]')) {
      const isLight = link.getAttribute("href")?.includes("light") ?? false;
      link.setAttribute("media", isLight === light ? "all" : "not all");
    }
  }, [theme]);
  return [theme, setTheme] as const;
}
