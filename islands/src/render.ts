import createDOMPurify, { type DOMPurify } from "dompurify";
import { Marked } from "marked";

// docs/adr/0068 §4: the island renders untrusted text (agent output, other
// members' messages, pages the agent read) inside the HOST's origin, so a
// rendering bug is script on opus.bnei.dev. Two layers: markdown never emits
// raw HTML, and everything it does emit goes through DOMPurify.

// https:, mailto: and relative URLs only. DOMPurify's default also allows
// http:, ftp:, tel: and others; none belongs in an agent transcript.
const ALLOWED_URI = /^(?:(?:https|mailto):|[^a-z]|[a-z+.-]+(?:[^a-z+.\-:]|$))/i;

const FORBID_TAGS = ["style", "form", "input", "button", "textarea", "select", "iframe", "object", "embed"];

const escape = (s: string) =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

const marked = new Marked({
  gfm: true,
  renderer: {
    // Raw HTML in markdown is shown as text, never parsed.
    html: ({ text }) => escape(text),
    // Mermaid is drawn later, lazily (Diagram.svelte); here it stays source.
    code({ text, lang }) {
      const cls = lang === "mermaid" ? ' class="mermaid-src"' : "";
      return `<pre${cls}><code>${escape(text)}</code></pre>`;
    },
  },
});

export type Renderer = {
  markdown(text: string): string;
  svg(svg: string): string;
};

// A factory so tests can pass a DOM window; the element passes the page's.
export function createRenderer(win: Window & typeof globalThis): Renderer {
  const purify: DOMPurify = createDOMPurify(win);
  // DOMPurify returns its input unchanged when it cannot run. Fail closed.
  if (!purify.isSupported) throw new Error("DOMPurify unsupported in this window");
  purify.addHook("afterSanitizeAttributes", (node) => {
    if (node.tagName === "A") {
      node.setAttribute("target", "_blank");
      node.setAttribute("rel", "noopener noreferrer");
    }
  });
  return {
    markdown: (text) =>
      purify.sanitize(marked.parse(text, { async: false }), {
        ALLOWED_URI_REGEXP: ALLOWED_URI,
        FORBID_TAGS,
        FORBID_ATTR: ["style"],
      }),
    // Mermaid already runs with securityLevel "strict"; its SVG is still
    // sanitized, because a mermaid bug is a host XSS too. Mermaid's own
    // <style> is kept (CSS runs no script, and it stays in the shadow root);
    // labels are SVG text, so foreignObject goes.
    svg: (svg) =>
      purify.sanitize(svg, {
        USE_PROFILES: { svg: true, svgFilters: true },
        ADD_TAGS: ["style"],
        ALLOWED_URI_REGEXP: ALLOWED_URI,
        FORBID_TAGS: ["form", "input", "button", "textarea", "select", "iframe", "object", "embed", "foreignObject", "script"],
      }),
  };
}
