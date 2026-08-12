import { useEffect } from "react";
import { useLocalStorageState } from "./useLocalStorageState";

export type Theme = "herd" | "herd-light";

// Key is duplicated in index.html's pre-paint script — that script is what
// stops a light-theme user seeing the dark default flash on every load, so if
// this key changes, change it there too.
const THEME_KEY = "herd.theme";

// The installed PWA paints its window chrome (status bar, task switcher) from
// theme-color, so it has to follow the theme or a light-theme install gets a
// black status bar. Same values as the themes' --color-base-100.
const THEME_COLOR: Record<Theme, string> = {
  herd: "#0f0e14",
  "herd-light": "#e9e5dd",
};

export function useTheme() {
  const [theme, setTheme] = useLocalStorageState<Theme>(THEME_KEY, "herd");
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.querySelector('meta[name="theme-color"]')?.setAttribute("content", THEME_COLOR[theme]);
  }, [theme]);
  return [theme, setTheme] as const;
}
