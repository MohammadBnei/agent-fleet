import { useCallback, useEffect, useRef, useState } from "react";

// Shared by the message feed and the TODOS card — both grow as new content
// streams in, and a reader scrolled up to review history shouldn't have to
// hunt for the end. Runs the check after every render (not just onScroll)
// so content growing while already at the bottom doesn't flip the button on.
export function useAtBottom<T extends HTMLElement>(threshold = 24) {
  const ref = useRef<T>(null);
  const [atBottom, setAtBottom] = useState(true);

  const checkAtBottom = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    setAtBottom(el.scrollHeight - el.scrollTop - el.clientHeight < threshold);
  }, [threshold]);

  useEffect(() => {
    checkAtBottom();
  });

  function scrollToBottom() {
    ref.current?.scrollTo({ top: ref.current.scrollHeight, behavior: "smooth" });
  }

  return { ref, atBottom, onScroll: checkAtBottom, scrollToBottom };
}
