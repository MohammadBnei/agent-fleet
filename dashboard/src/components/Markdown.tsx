import { lazy, memo, Suspense, useMemo } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

// Module-level, not an inline literal: a fresh array on every render
// invalidates ReactMarkdown's internal cache for the same reason a fresh
// components object does (see useMarkdownComponents below). GFM is what turns
// on tables — the agent writes them constantly and stock CommonMark rendered
// them as a wall of pipes — plus strikethrough, task lists and autolinks.
const REMARK_PLUGINS = [remarkGfm];

// Lazy, because mermaid is the single largest thing the dashboard ships and
// almost nothing renders a diagram. Imported statically it put mermaid's core
// (plus its top-level mermaid.initialize()) in the first-visit chunk of every
// session that renders any markdown at all — which is all of them. Its own
// diagram renderers, cytoscape and katex were already separate chunks; this is
// the part that wasn't.
const MermaidDiagram = lazy(() =>
  import("./MermaidDiagram").then((m) => ({ default: m.MermaidDiagram })),
);

// Hand-styled to match the existing text-sm text-base-content
// message-bubble baseline (no @tailwindcss/typography — this app has one
// hardcoded-dark DaisyUI theme, prose/prose-invert buys nothing here).
// Using useMemo to avoid recreating this object on every render, which
// triggers ReactMarkdown's internal cache invalidation.
function useMarkdownComponents(): Components {
  return useMemo<Components>(
    () => ({
      p: (props) => <p className="mb-2 last:mb-0" {...props} />,
      ul: (props) => <ul className="pl-4 list-disc mb-2" {...props} />,
      // The gutter is sized in `ch` because the body font is IBM Plex Mono:
      // a marker is "10." wide at worst, plus the browser's own marker gap,
      // and 4ch covers that exactly at any font size. pl-4 clipped the digit
      // outright; pl-5 (a fixed 20px) still cut it roughly in half on a
      // phone — visible in any numbered plan, which is most of them. Guessing
      // a px gutter for a proportional-ish marker is what kept getting this
      // wrong; `ch` measures the thing actually being fitted.
      ol: (props) => <ol className="pl-[4ch] list-decimal mb-2" {...props} />,
      li: (props) => <li className="mb-0.5" {...props} />,
      a: (props) => <a className="text-primary underline" target="_blank" rel="noreferrer" {...props} />,
      h1: (props) => <h1 className="font-semibold text-base mt-2 mb-1" {...props} />,
      h2: (props) => <h2 className="font-semibold text-base mt-2 mb-1" {...props} />,
      h3: (props) => <h3 className="font-semibold text-base mt-2 mb-1" {...props} />,
      // Also the human-message treatment — see asDisplayMarkdown in transcript.ts,
      // which is the only thing that ever produces a blockquote in this app.
      // Colored to match the composer's ">"/Send button (text-primary) — this
      // is the actual color human messages render in; a wrapping div's color
      // class has no effect here since this element sets its own.
      blockquote: (props) => (
        <blockquote className="border-l-2 border-primary/30 pl-2 italic text-primary" {...props} />
      ),
      // A table is the one block element here that has its own intrinsic
      // minimum width, so it gets the overflow-x-auto wrapper rather than
      // being allowed to widen the feed column — the same min-w-0 trap
      // SessionPanels.tsx documents, which silently pushed a mobile column
      // to 437px on a 390px viewport. w-max keeps the table its natural
      // width inside the scroller instead of squashing every column.
      table: (props) => (
        <div className="overflow-x-auto my-2">
          <table className="w-max min-w-full border border-line3 text-xs" {...props} />
        </div>
      ),
      th: (props) => (
        <th className="border border-line3 px-2 py-1 text-left font-semibold text-dim2 align-top" {...props} />
      ),
      td: (props) => <td className="border border-line3 px-2 py-1 align-top" {...props} />,
      // Unwrapped so the `code` renderer below fully owns fenced-block markup
      // (needed for mermaid, which renders a <div>, not a <pre><code>).
      pre: ({ children }) => <>{children}</>,
      code({ className, children }) {
        const lang = /language-(\w+)/.exec(className ?? "")?.[1];
        const text = String(children).replace(/\n$/, "");
        // fallback={null}: the diagram appears when its chunk lands. A
        // spinner here would flash on every fence in a long transcript.
        if (lang === "mermaid")
          return (
            <Suspense fallback={null}>
              <MermaidDiagram code={text} />
            </Suspense>
          );
        if (lang) {
          return (
            <pre className="bg-base-300/40 rounded-md px-3 py-2.5 overflow-x-auto font-mono text-xs my-2">
              <code>{text}</code>
            </pre>
          );
        }
        return <code className="bg-base-content/10 rounded px-1.5 py-0.5 font-mono text-xs">{children}</code>;
      },
    }),
    [], // Empty deps - components object is stable
  );
}

// Memoized to prevent re-rendering when parent re-renders but text hasn't changed.
// Critical for performance when rendering many messages - without this, every new
// message causes all previous Markdown components to re-parse their content.
export const Markdown = memo(function Markdown({ text }: { text: string }) {
  const components = useMarkdownComponents();
  return (
    <ReactMarkdown components={components} remarkPlugins={REMARK_PLUGINS}>
      {text}
    </ReactMarkdown>
  );
});
