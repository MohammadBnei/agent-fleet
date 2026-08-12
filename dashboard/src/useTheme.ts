import { useEffect } from "react";
import { useLocalStorageState } from "./useLocalStorageState";

export type Theme = "herd" | "herd-light";

// Key is duplicated in index.html's pre-paint script — that script is what
// stops a light-theme user seeing the dark default flash on every load, so if
// this key changes, change it there too.
const THEME_KEY = "herd.theme";

export function useTheme() {
  const [theme, setTheme] = useLocalStorageState<Theme>(THEME_KEY, "herd");
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);
  return [theme, setTheme] as const;
}
