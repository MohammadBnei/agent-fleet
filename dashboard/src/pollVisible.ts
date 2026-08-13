// setInterval, minus the ticks nobody can see.
//
// A backgrounded mobile browser suspends the network but keeps timers
// firing, so every 5s poll rejected with "Failed to fetch" while the phone
// was in a pocket — which App.tsx then raised as a page-level ErrorModal.
// Skipping hidden ticks removes the failures at the source rather than
// teaching each caller to swallow them, and the visible-edge fire means
// coming back to the tab shows fresh data immediately instead of after up
// to one full interval of staleness.
//
// ponytail: document.hidden only. A tab that's visible but occluded still
// polls — the browser gives us nothing better, and the cost is one small
// RPC.
export function pollVisible(fn: () => void, ms: number): () => void {
  const tick = () => {
    if (!document.hidden) fn();
  };
  const onVisibility = () => {
    if (!document.hidden) fn();
  };

  tick();
  const timer = setInterval(tick, ms);
  document.addEventListener("visibilitychange", onVisibility);

  return () => {
    clearInterval(timer);
    document.removeEventListener("visibilitychange", onVisibility);
  };
}
