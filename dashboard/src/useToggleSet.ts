import { useCallback, useState } from "react";

// A Set of strings you flip membership in — the shape every filter and
// selection in the list views wanted, hand-rolled five times before this
// (two repo filters, two status filters, the batch selection) as the same
// six lines of copy-on-write:
//
//   setX((prev) => { const next = new Set(prev); next.has(v) ? next.delete(v) : next.add(v); return next; })
//
// `toggle` is stable across renders (functional update, nothing closed over),
// so callers can put it in a dependency array without defeating the memo.
export function useToggleSet(initial?: Iterable<string>) {
  const [set, setSet] = useState<Set<string>>(() => new Set(initial));
  const toggle = useCallback((value: string) => {
    setSet((prev) => {
      const next = new Set(prev);
      if (next.has(value)) next.delete(value);
      else next.add(value);
      return next;
    });
  }, []);
  const clear = useCallback(() => setSet(new Set()), []);
  return { set, toggle, clear, setSet } as const;
}
