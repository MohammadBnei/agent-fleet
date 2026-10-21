// Draws ```mermaid blocks after render, loading mermaid only when one exists.
// securityLevel "strict" plus a DOMPurify pass on the SVG (docs/adr/0068 §4).
// If the chunk is missing (a page still on the previous release's entry) or
// the diagram fails, the source stays as code.

let mermaid: Promise<typeof import("mermaid").default> | undefined;
let n = 0;

export function drawDiagrams(node: HTMLElement, opts: { items: unknown; svg: (s: string) => string }) {
  let svg = opts.svg;
  const draw = async () => {
    const blocks = node.querySelectorAll<HTMLPreElement>("pre.mermaid-src:not([data-drawn])");
    if (!blocks.length) return;
    try {
      mermaid ??= import("mermaid").then((m) => m.default);
      const m = await mermaid;
      // The host's Ukubi theme picks mermaid's: light office, dark tunnels.
      const theme = (node.getRootNode() as ShadowRoot).host?.getAttribute("theme");
      m.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        theme: theme === "underground" || theme === "glow" ? "dark" : "neutral",
        htmlLabels: false,
        flowchart: { htmlLabels: false },
      });
      for (const pre of blocks) {
        pre.dataset.drawn = "";
        try {
          const out = await m.render(`fleet-diagram-${++n}`, pre.textContent ?? "");
          const fig = document.createElement("figure");
          fig.className = "diagram";
          fig.innerHTML = svg(out.svg);
          pre.replaceWith(fig);
        } catch {
          // Not a valid diagram: leave the source.
        }
      }
    } catch {
      mermaid = undefined;
    }
  };
  queueMicrotask(draw);
  return {
    update(next: { items: unknown; svg: (s: string) => string }) {
      svg = next.svg;
      queueMicrotask(draw);
    },
  };
}
