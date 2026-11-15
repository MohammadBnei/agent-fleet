// Show the result first, send second, undo if the send fails. The shape every
// "click, then wait on a warm" surface wants: `apply` runs before the RPC so
// the UI answers the click at once, `rollback` runs with the error only if it
// rejects. Resolves to whether the call succeeded.
export async function optimistic(
  apply: () => void,
  action: () => Promise<unknown>,
  rollback: (err: Error) => void,
): Promise<boolean> {
  apply();
  try {
    await action();
    return true;
  } catch (err) {
    rollback(err instanceof Error ? err : new Error(String(err)));
    return false;
  }
}
