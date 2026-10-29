import { useCallback, useState } from "react";

// Generic localStorage-backed state for per-viewer preferences (theme, density,
// feed width) so they survive a reload. Lazy
// initializer reads once on mount (no flash of default layout); every set
// writes through synchronously, cheap for the small values used here.
export function useLocalStorageState<T>(key: string, initial: T) {
  const [value, setValue] = useState<T>(() => {
    try {
      const raw = localStorage.getItem(key);
      return raw ? (JSON.parse(raw) as T) : initial;
    } catch {
      return initial;
    }
  });

  // Stable across renders (functional update only, no closed-over `value`)
  // so callers can safely put it in a useCallback/useEffect dependency
  // array without that memoization being defeated every render.
  const set = useCallback(
    (next: T | ((prev: T) => T)) => {
      setValue((prev) => {
        const resolved =
          typeof next === "function" ? (next as (p: T) => T)(prev) : next;
        try {
          localStorage.setItem(key, JSON.stringify(resolved));
        } catch {
          // storage full/disabled — layout just won't persist this time
        }
        return resolved;
      });
    },
    [key],
  );

  return [value, set] as const;
}
