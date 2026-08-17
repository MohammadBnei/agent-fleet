import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

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
  //
  // The distance from the bottom is what's stable across a prepend, so that
  // is what's recorded — and it is restored in the layout effect below, NOT
  // in the promise's own continuation. Awaiting the fetch only means React
  // has been *told* about the new entries; a requestAnimationFrame there fires
  // against a DOM mid-swap, where scrollHeight briefly equals clientHeight and
  // the browser clamps scrollTop to 0. Measured: the feed jumped to the very
  // top, which is the bug this whole change exists to fix.
  const pendingAnchor = useRef<number | null>(null);
  const anchorPrepend = useCallback(async (load: () => Promise<void>) => {
    const el = ref.current;
    pendingAnchor.current = el ? el.scrollHeight - el.scrollTop : null;
    try {
      await load();
    } catch (err) {
      pendingAnchor.current = null;
      throw err;
    }
  }, []);

  // After the commit that added the entries, before paint.
  useLayoutEffect(() => {
    const fromBottom = pendingAnchor.current;
    if (fromBottom === null) return;
    const el = ref.current;
    if (!el) return;
    // Nothing has been laid out yet if the content hasn't grown — leave the
    // anchor pending and try again on the next commit.
    if (el.scrollHeight <= fromBottom) return;
    el.scrollTop = el.scrollHeight - fromBottom;
    pendingAnchor.current = null;
  });

  return { ref, atBottom, onScroll: checkAtBottom, scrollToBottom, anchorPrepend };
}
