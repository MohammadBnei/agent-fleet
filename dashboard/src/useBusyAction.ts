import { useCallback, useRef, useState } from "react";
import { errorText } from "./agentErrors";

// "Run one RPC, remember which button fired it, surface the failure" — the
// contract every action surface in the console wants, written twice before
// this (useSessionDetail's detail-view actions, SessionActionsModal's list-row
// ones) with the same body and different names for the same two fields.
//
// busyKey rather than a boolean: a click on one card used to disable every
// other card and the whole actions menu at once, so the spinner landed
// nowhere near the thing that had been clicked. Keying it means the feedback
// lands on the button that was actually pressed, and callers that genuinely
// want page-level disabling still have `busyKey !== null`.
//
// Several actions can be in flight at once (a slow warm must not lock an
// unrelated card), so the in-flight keys are a list: busyKey is the newest of
// them, and a second run() under a key that is already in flight is refused
// rather than sent twice.
//
// run() resolves to whether the call succeeded, so a caller holding optimistic
// state knows whether to keep or roll it back, and a caller that just wants to
// reload on success can branch on it. Callers that care about neither ignore it.
export function useBusyAction() {
  const [busyKeys, setBusyKeys] = useState<string[]>([]);
  const inFlight = useRef(new Set<string>());
  const [error, setError] = useState<string | null>(null);

  const run = useCallback(
    async (action: () => Promise<unknown>, key: string): Promise<boolean> => {
      if (inFlight.current.has(key)) return false;
      inFlight.current.add(key);
      setBusyKeys((prev) => [...prev, key]);
      setError(null);
      try {
        await action();
        return true;
      } catch (err) {
        setError(errorText(err));
        return false;
      } finally {
        inFlight.current.delete(key);
        setBusyKeys((prev) => prev.filter((k) => k !== key));
      }
    },
    [],
  );

  const clearError = useCallback(() => setError(null), []);
  const busyKey = busyKeys.length > 0 ? busyKeys[busyKeys.length - 1] : null;
  const isBusy = useCallback(
    (key: string) => busyKeys.includes(key),
    [busyKeys],
  );
  return {
    busyKey,
    busyKeys,
    isBusy,
    error,
    run,
    clearError,
    setError,
  } as const;
}
