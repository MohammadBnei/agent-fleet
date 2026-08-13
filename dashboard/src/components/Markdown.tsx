import { memo, useMemo } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import { MermaidDiagram } from "./MermaidDiagram";

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
      // Unwrapped so the `code` renderer below fully owns fenced-block markup
      // (needed for mermaid, which renders a <div>, not a <pre><code>).
      pre: ({ children }) => <>{children}</>,
      code({ className, children }) {
        const lang = /language-(\w+)/.exec(className ?? "")?.[1];
        const text = String(children).replace(/\n$/, "");
        if (lang === "mermaid") return <MermaidDiagram code={text} />;
        if (lang) {
          return (
            <pre className="bg-base-300/40 rounded-md p-2 overflow-x-auto font-mono text-xs my-2">
              <code>{text}</code>
            </pre>
          );
        }
        return <code className="bg-base-content/10 rounded px-1 font-mono text-xs">{children}</code>;
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
  return <ReactMarkdown components={components}>{text}</ReactMarkdown>;
});
