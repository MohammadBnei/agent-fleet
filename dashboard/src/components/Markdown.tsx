import ReactMarkdown, { type Components } from "react-markdown";
import { MermaidDiagram } from "./MermaidDiagram";

// Hand-styled to match the existing text-[12px] text-base-content/90
// message-bubble baseline (no @tailwindcss/typography — this app has one
// hardcoded-dark DaisyUI theme, prose/prose-invert buys nothing here).
const components: Components = {
  p: (props) => <p className="mb-2 last:mb-0" {...props} />,
  ul: (props) => <ul className="pl-4 list-disc mb-2" {...props} />,
  ol: (props) => <ol className="pl-4 list-decimal mb-2" {...props} />,
  li: (props) => <li className="mb-0.5" {...props} />,
  a: (props) => <a className="text-primary underline" target="_blank" rel="noreferrer" {...props} />,
  h1: (props) => <h1 className="font-semibold text-[13px] mt-2 mb-1" {...props} />,
  h2: (props) => <h2 className="font-semibold text-[13px] mt-2 mb-1" {...props} />,
  h3: (props) => <h3 className="font-semibold text-[13px] mt-2 mb-1" {...props} />,
  // Also the human-message treatment — see asDisplayMarkdown in transcript.ts,
  // which is the only thing that ever produces a blockquote in this app.
  blockquote: (props) => (
    <blockquote className="border-l-2 border-base-content/20 pl-2 italic text-base-content/70" {...props} />
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
        <pre className="bg-base-300/40 rounded-md p-2 overflow-x-auto font-mono text-[11px] my-2">
          <code>{text}</code>
        </pre>
      );
    }
    return <code className="bg-base-content/10 rounded px-1 font-mono text-[11px]">{children}</code>;
  },
};

export function Markdown({ text }: { text: string }) {
  return <ReactMarkdown components={components}>{text}</ReactMarkdown>;
}
