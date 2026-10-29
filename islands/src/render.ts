import createDOMPurify, { type DOMPurify } from "dompurify";
import { Marked } from "marked";

// docs/adr/0068 §4: the island renders untrusted text (agent output, other
// members' messages, pages the agent read) inside the HOST's origin, so a
// rendering bug is script on opus.bnei.dev. Two layers: markdown never emits
// raw HTML, and everything it does emit goes through DOMPurify.

// DOMPurify checks EVERY allowed attribute's value against this, not only
// URLs, so it must pass plain values (class, viewBox): it is DOMPurify's
// default minus http:, ftp:, tel: and the rest. The link hook below then
// keeps only absolute https: and mailto: links: a relative URL would resolve
// against the HOST and go out with its cookies (docs/adr/0068 §4).
const ALLOWED_URI = /^(?:(?:https|mailto):|[^a-z]|[a-z+.-]+(?:[^a-z+.\-:]|$))/i;
const LINK = /^(?:https|mailto):/i;
const URL_ATTRS = ["href", "xlink:href", "src", "action", "formaction"];

// No <img>: an image loads on render, a zero-click beacon. Images become links.
const FORBID_TAGS = [
  "style",
  "form",
  "input",
  "button",
  "textarea",
  "select",
  "iframe",
  "object",
  "embed",
  "img",
];

const escapeHtml = (s: string) =>
  s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");

const marked = new Marked({
  gfm: true,
  renderer: {
    // Raw HTML in markdown is shown as text, never parsed.
    html: ({ text }) => escapeHtml(text),
    image: ({ href, text }) =>
      `<a href="${escapeHtml(href)}">${escapeHtml(text || href)}</a>`,
    // Mermaid is drawn later, lazily (diagram.ts); here it stays source.
    code({ text, lang }) {
      const cls = lang === "mermaid" ? ' class="mermaid-src"' : "";
      return `<pre${cls}><code>${escapeHtml(text)}</code></pre>`;
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
  if (!purify.isSupported)
    throw new Error("DOMPurify unsupported in this window");
  purify.addHook("afterSanitizeAttributes", (node) => {
    for (const a of URL_ATTRS) {
      const v = node.getAttribute(a);
      if (v !== null && !LINK.test(v.trim())) node.removeAttribute(a);
    }
    if (node.tagName.toLowerCase() === "a") {
      node.setAttribute("target", "_blank");
      node.setAttribute("rel", "noopener noreferrer");
    }
  });
  // Mermaid's own <style> is kept in the SVG path. Anything that could fetch
  // is refused outright, escapes included (u\72l( spells url( to CSS):
  // backslashes, image-set, @import, @font-face, src:, and any url() that
  // does not point at #a-marker inside the SVG.
  purify.addHook("uponSanitizeElement", (node, data) => {
    if (data.tagName !== "style") return;
    const css = node.textContent ?? "";
    if (
      /\\|image-set|@import|@font-face|\bsrc\s*:|expression\(/i.test(css) ||
      /url\(\s*['"]?\s*[^'"#\s]/i.test(css)
    ) {
      node.parentNode?.removeChild(node);
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
        FORBID_TAGS: [
          "form",
          "input",
          "button",
          "textarea",
          "select",
          "iframe",
          "object",
          "embed",
          "foreignObject",
          "script",
          "image",
          "img",
        ],
      }),
  };
}
