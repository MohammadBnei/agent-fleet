import { useCallback, useEffect, useRef, useState } from "react";

// Shared by the message feed and the TODOS card — both grow as new content
// streams in, and a reader scrolled up to review history shouldn't have to
// hunt for the end. Runs the check after every render (not just onScroll)
// so content growing while already at the bottom doesn't flip the button on.
export function useAtBottom<T extends HTMLElement>(threshold = 100) {
  const ref = useRef<T>(null);
  const [atBottom, setAtBottom] = useState(true);

  const checkAtBottom = useCallback(() => {
    const el = ref.current;
    if (!el) return;

    // Use requestAnimationFrame for more reliable scroll measurements,
    // especially on mobile where scroll events can fire before layout completes
    requestAnimationFrame(() => {
      if (!el) return;
      const isAtBottom = el.scrollHeight - el.scrollTop - el.clientHeight < threshold;
      setAtBottom(isAtBottom);
    });
  }, [threshold]);

  useEffect(() => {
    checkAtBottom();
  });

  // useCallback, so an effect can honestly depend on it: as a plain function
  // this got a new identity every render, which quietly made every effect
  // listing it in its deps an every-render effect.
  const scrollToBottom = useCallback(() => {
    ref.current?.scrollTo({ top: ref.current.scrollHeight, behavior: "smooth" });
  }, []);

  // Runs a load-older fetch with the scroll position pinned to whatever the
  // reader is looking at. Prepending history grows scrollHeight above the
  // viewport, which would otherwise shove the current entry down the screen
  // by exactly the height of the page just loaded.
  const anchorPrepend = useCallback(async (load: () => Promise<void>) => {
    const el = ref.current;
    const before = el ? el.scrollHeight - el.scrollTop : 0;
    await load();
    if (!el) return;
    // After paint: React has committed the new entries by the time the
    // promise resolves, but the browser has not necessarily laid them out.
    requestAnimationFrame(() => {
      el.scrollTop = el.scrollHeight - before;
    });
  }, []);

  return { ref, atBottom, onScroll: checkAtBottom, scrollToBottom, anchorPrepend };
}
