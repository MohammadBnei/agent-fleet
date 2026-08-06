import { useEffect, useId, useRef, useState } from "react";
import mermaid from "mermaid";

mermaid.initialize({ startOnLoad: false, theme: "dark" });

// Renders a ```mermaid fence from Markdown.tsx as an actual diagram —
// mermaid.render() is async and returns raw SVG markup, so this needs its
// own client-side effect rather than a synchronous component.
export function MermaidDiagram({ code }: { code: string }) {
  const id = useId().replace(/:/g, "-");
  const ref = useRef<HTMLDivElement>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    mermaid
      .render(`mermaid-${id}`, code)
      .then(({ svg }) => {
        if (!cancelled && ref.current) ref.current.innerHTML = svg;
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err));
      });
    return () => {
      cancelled = true;
    };
  }, [id, code]);

  if (error) {
    return <pre className="text-[10.5px] font-mono whitespace-pre-wrap text-error my-2">{error}</pre>;
  }
  return <div ref={ref} className="my-2 [&_svg]:max-w-full" />;
}
