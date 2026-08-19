import { useCallback, useState } from "react";

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
// run() resolves to whether the call succeeded, so a caller holding optimistic
// state knows whether to keep or roll it back, and a caller that just wants to
// reload on success can branch on it. Callers that care about neither ignore it.
export function useBusyAction() {
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const run = useCallback(async (action: () => Promise<unknown>, key: string): Promise<boolean> => {
    setBusyKey(key);
    setError(null);
    try {
      await action();
      return true;
    } catch (err) {
      setError((err as Error).message);
      return false;
    } finally {
      setBusyKey(null);
    }
  }, []);

  const clearError = useCallback(() => setError(null), []);
  return { busyKey, error, run, clearError, setError } as const;
}
