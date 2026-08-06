import { useState } from "react";

// Generic localStorage-backed state — used for the right panel's card order
// and per-card heights so a reorganized dashboard survives a reload. Lazy
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

  function set(next: T | ((prev: T) => T)) {
    setValue((prev) => {
      const resolved = typeof next === "function" ? (next as (p: T) => T)(prev) : next;
      try {
        localStorage.setItem(key, JSON.stringify(resolved));
      } catch {
        // storage full/disabled — layout just won't persist this time
      }
      return resolved;
    });
  }

  return [value, set] as const;
}
