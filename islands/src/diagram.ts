// Draws ```mermaid blocks after render, loading mermaid only when one exists.
// securityLevel "strict" plus a DOMPurify pass on the SVG (docs/adr/0068 §4).
// If the chunk is missing (a page still on the previous release's entry) or
// the diagram fails, the source stays as code.

let mermaid: Promise<typeof import("mermaid").default> | undefined;

// Diagram text is untrusted (§4). Directives and front-matter config can set
// themeCSS and other options, so both go before mermaid reads it: every line
// holding "%%{" (closed or not, mermaid accepts both), and every leading
// front-matter block, until nothing changes, so a removal cannot assemble a
// new one.
export function stripConfig(src: string): string {
  for (;;) {
    const next = src
      .split(/\r?\n/)
      .filter((line) => !line.includes("%%{"))
      .join("\n")
      .replace(/^\s*---[\s\S]*?\n\s*---\s*(\n|$)/, "");
    if (next === src) return next;
    src = next;
  }
}

export function drawDiagrams(node: HTMLElement, opts: { items: unknown; svg: (s: string) => string }) {
  let svg = opts.svg;
  const root = node.getRootNode() as ShadowRoot;
  const draw = async () => {
    const blocks = node.querySelectorAll<HTMLPreElement>("pre.mermaid-src:not([data-drawn])");
    if (!blocks.length) return;
    try {
      mermaid ??= import("mermaid").then((m) => m.default);
      const m = await mermaid;
      // The host's Ukubi theme picks mermaid's: light office, dark tunnels.
      const theme = root.host?.getAttribute("theme");
      m.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        theme: theme === "underground" || theme === "glow" ? "dark" : "neutral",
        themeCSS: "",
        suppressErrorRendering: true,
        htmlLabels: false,
        flowchart: { htmlLabels: false },
      });
      for (const pre of blocks) {
        pre.dataset.drawn = "";
        // Mermaid lays out in a temporary node of its own in the host's
        // body. With error rendering suppressed it removes that node on
        // success and on failure alike, so nothing is left behind.
        try {
          const out = await m.render(`fleet-diagram-${crypto.randomUUID()}`, stripConfig(pre.textContent ?? ""));
          // Each diagram gets its own shadow root, so its <style> styles only
          // the diagram, and paint containment keeps anything it draws, fixed
          // positioning included, inside its box.
          const fig = document.createElement("div"); // <figure> cannot host a shadow root
          fig.className = "diagram";
          fig.attachShadow({ mode: "open" }).innerHTML = svg(out.svg);
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
