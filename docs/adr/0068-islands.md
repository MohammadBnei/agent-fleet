# ADR-0068 — Islands: fleet UI as web components in other apps

- **Status:** Proposed
- **Date:** 2026-10-21
- **Related:** [ADR-0066](0066-agent-service.md) (AgentService; this ADR
  amends its §1 and the `/embed/*` mentions in §2),
  [ADR-0067](0067-shared-and-private-sessions.md) (visibility and authorship
  the island shows), [ADR-0062](0062-defined-agents.md) (`embed.origins` on a
  definition), [ADR-0069](0069-component-auth-and-network-fences.md)
  (`frame-ancestors`), infra-bootstrap ADR-0058 (SilverBullet at
  `notes.bnei.dev`, the Opus space at `opus.bnei.dev`).

## Context

N MG members will work with the `opus-workspace` agent from inside the Opus
SilverBullet space: start a thread, talk to it, answer its questions and
approve its plans and permissions, while the page they are editing is right
there. ADR-0066 and ADR-0067 give them an API (`AgentService`, member login,
shared and private threads, per-entry authors). They have no UI outside the
admin console, which members can never reach.

The plan (P5) was an iframe: `fleet.bnei.dev/embed/<definition>`, a static
shell carrying no data, framed by the host under a per-definition
`frame-ancestors`, with login in a popup. The iframe keeps the agent's page
and the approve button in different origins.

The owner rejected the iframe on four counts:

- it does not fit: a fixed box, double scrollbars, styling that is not the
  host's;
- login inside a frame is fragile (framing refused by authentik, opener
  handshakes);
- the host cannot integrate with it (the current page, host events);
- an island should be a real component dropped into any app, plain HTML or
  React, not a window onto another site.

## Decision

### 1. An island is a web component served by core

- Islands are custom elements, built with **Svelte 5** compiled to custom
  elements (`customElement: true`). The runtime is small, and the same
  element works from a plain `<script type="module">` (SilverBullet, any
  page) and in React 19, which renders custom elements natively.
- The first island is `<fleet-agent definition="opus-workspace">`: thread
  list (shared plus the member's private ones), the feed, inline questions,
  plan approval and permission prompts, the config bar within the
  definition's `allow` lists, and the status chip.
- One ES module bundle per island under `fleet.bnei.dev/islands/`, built from
  `islands/` in this repo and served by core as static files. Heavy renderers
  (mermaid) load lazily.
- The island shares plain TypeScript with the console (transcript parsing,
  the generated `AgentService` client and the SDK helpers), not its React
  components. The UI is written a second time, in Svelte.
- **The look is the Ukubi design system** (the organization's default in
  Claude Design, https://claude.ai/artifact/B8nzjukaMY8HRQnjLRRKMc): its
  three themes, Readex Pro and JetBrains Mono, stone radii, the chamfer on
  the send button and on the "needs you" card, and the LED, lit only while
  the thread waits on a human (a question, a plan, a permission). Its tokens
  are copied into `islands/` as CSS custom properties; that artifact stays
  the source, and a copy is refreshed by PR. Fonts are declared in the host
  document (`@font-face` does not apply inside a shadow root): the element
  adds the Google Fonts link to `<head>` once, if the host has not.
- It calls **`AgentService` only.** A lint import guard keeps `islands/` off
  `DashboardService` and off `dashboard/src` components.

### 2. Host API

- **Attributes:** `definition` (required), `context` (the host's current
  page path, passed to `CreateThread` as a labelled reference, validated
  server-side as a workspace path), `thread` (open a given thread).
- **Events** (DOM `CustomEvent`, bubbling, composed): `fleet-status`
  (`loading | working | waiting | idle | error`), `fleet-message` (a new
  agent entry, its id and kind only).
- **Theme:** a `theme` attribute (`office`, the default, `underground` or
  `glow`). Every colour, size and radius is a `--ukubi-*` custom property,
  which CSS inheritance carries into the Shadow DOM, so a host can override
  any of them.
- Nothing else. The host cannot call methods that act on the session; the
  element acts only on its own user's clicks.

### 3. Auth: the fleet cookie, same-site hosts, CORS on AgentService only

- **Same-site hosts only.** `opus.bnei.dev` and `fleet.bnei.dev` are the same
  site (`bnei.dev` is the registrable domain, both on https), so the
  `__Host-` `SameSite=Lax` session cookie rides a credentialed `fetch` and is
  not a third-party cookie that browsers block. The definitions validator
  already limits `embed.origins` to exact `https://` origins on `bnei.dev` or
  `*.bnei.dev`.
- **Where CORS sits.** The handler order is `FrameDeny` → CORS → `OIDC.Gate`
  → mux. The CORS layer matches the `AgentService` path prefix on
  `EscapedPath()`, the string the Gate and mux use, and nothing else:
  - it answers the preflight itself, without auth (a preflight never carries
    cookies);
  - it decorates every `AgentService` response, the Gate's own 401 and 403
    included, so the island sees `Unauthenticated` and not an opaque network
    error;
  - for an allowlisted `Origin` it echoes that exact origin with
    `Access-Control-Allow-Credentials: true`; `Vary: Origin` goes on every
    `AgentService` response, CORS or not;
  - `Access-Control-Allow-Headers` lists the Connect headers and
    `X-Fleet-Dashboard`, the CSRF header both services share, and only on the
    AgentService prefix. A preflight is answered per path, and every other
    path (`DashboardService` and `/auth/*` included) sends no CORS header, so
    a cross-origin call there still dies at its unanswered preflight.
- **What `Origin` means** (amends ADR-0066 §1 and §4). `Origin` is a browser
  signal; a non-browser caller can set anything, and on `:8080` it still needs
  a member cookie.
  - **Absent, or the fleet's own origin:** the console. ADR-0066's rules
    apply unchanged.
  - **An origin some valid definition lists:** an island call. The caller is
    evaluated **as a member only**: `platform-admins` gives nothing extra, so
    a script on a host can never use an admin's cookie as admin power. Every
    method is fenced to the definitions listing that origin: a
    session-scoped method (streams included) resolves the session's
    definition, list methods are filtered, and a mismatch is the same
    `NotFound` as an unseen session. An origin allowed for one definition
    cannot reach another.
  - **Anything else, `null` included:** refused, by the CORS layer itself
    with a 403 before the Gate or any handler runs: a refused preflight alone
    does not stop a simple POST. A non-browser caller sending an odd
    `Origin` is refused too; machine clients belong on `:9091`. AgentService's interceptor refuses the same
    origins again as a second layer.
  The CSRF header stays mandatory on every `AgentService` call, unary GETs
  included.
- **Static bundles.** `/islands/*` becomes the gate's first exempt prefix
  (the other exemptions are exact paths), matched on the same escaped string
  the file server uses, with a path-traversal test. The entry module
  (`/islands/fleet-agent.js`) has a stable URL and `Cache-Control: no-cache`
  with an ETag, so a fix or rollback reaches hosts at once. It is one file;
  the only lazy chunk is mermaid, content-hashed and `immutable`. A page that
  still runs the previous entry after a release can miss that chunk, and then
  shows the diagram's source instead. All of it is served with
  `Access-Control-Allow-Origin: *`: it holds no data, and module scripts load
  in CORS mode.
- **Login.** On `Unauthenticated` the element shows "Sign in", which opens
  `fleet.bnei.dev/auth/login?popup=1` top-level in a popup. `popup=1` only
  selects the fixed local landing page `/auth/done`, which joins the exempt
  paths; the return target stays local-only (`safeReturnTo`). The landing page
  closes itself. The element does not rely on `window.opener`: it retries
  when its window regains focus and offers a manual "Retry". When the popup
  is blocked, "Sign in" opens a new tab instead. Core never sets
  `Cross-Origin-Opener-Policy: same-origin` on `/auth/*`; authentik's pages
  are outside its control and only break the opener, which nothing needs.
- **No iframe, so no `/embed/*`.** `frame-ancestors 'none'` (ADR-0069)
  stays on every fleet path. The Allow button now lives in the host page, so
  the framing defence moves there: **a host in `embed.origins` must send
  `frame-ancestors 'self'`**, or a same-site page (a preview host, any
  `*.bnei.dev` app) could frame it and clickjack an approval with the cookie
  riding.
- **Which origins.** An origin is listed only for a host the platform serves
  as a registered app (infra-bootstrap `gitops/apps/registry.yaml`), and
  leaves the definition in the PR that removes the app: wildcard DNS means a
  dropped name still resolves, and anyone who later claims it would inherit
  the access. Definitions ship at the release tag (ADR-0062), so removing an
  origin takes effect at the next release; until then the Gate's 401 is the
  only stop.

### 4. The boundary is the host origin's scripts

**The cookie authenticates the browser, not the gesture. Any script running
on an allowlisted origin acts as the signed-in member while the page is
open.** `HttpOnly` stops a script from reading the cookie; it does not stop
one from sending it, and the CSRF header does not help, since a script on an
allowed origin can set it too. Shadow DOM and `event.isTrusted` are not
boundaries either: the script calls the API, not the element.

- All decisions are inline, the riskiest included. There is no fleet-origin
  confirmation step.
- **Who can put a script on the host.**
  - The agent: on a host it writes to (the Opus space is its workspace), the
    **script-write ask** (P5 spike 5) asks a human for any write of
    `.js`/`.mjs`, a `space-lua`/`space-script` fence, `${`, `js.import`,
    `js.window` or a `Library/` install, even in `auto`.
  - Any member who can edit the space, through SilverBullet itself. The ask
    does not cover humans.
  - Whoever ships a fleet release: the island bundle runs in every host. That
    is the same trust as the core image, an owner-merged PR with green CI.
- **The island renders untrusted content inside the host origin**: agent
  output, other members' messages in shared threads, text the agent read from
  the web or the space. A rendering bug there is script in the host. So:
  markdown renders with raw HTML off, every produced HTML string goes through
  DOMPurify (default profile, no `style`, no `<form>`, URLs limited to
  `https:`, `mailto:` and relative) before it reaches the DOM, links get
  `rel="noopener noreferrer"`, and mermaid runs with
  `securityLevel: "strict"`. A test renders a fixed XSS corpus (script tags,
  event handlers, `javascript:` links, SVG payloads, mermaid click
  directives) and asserts no script runs.
- **Accepted residual:** one approved or member-written script, a sanitizer
  bug, or any XSS on an allowlisted host can act as every member who opens
  that page, within that member's rights on the host's definitions:
  answering, approving, posting, reading their threads. Admin power is out of
  reach (island calls are member-only). Group removal takes effect when the
  12h cookie expires (ADR-0069 §5).
- An origin goes into `embed.origins` only when its scripts are reviewed or
  guarded like this. Adding one is a definition PR.

## Alternatives considered

- **An iframe at `/embed/<definition>`** (the P5 plan): origin isolation
  keeps the agent's scripts away from the approve button. Rejected by the
  owner for fit, login, host integration and composability (Context).
- **A custom element wrapping the console's React components:** no second
  UI, but about 150 KB gzipped with React, in every host page. Rejected for
  weight.
- **An npm React library only:** best inside React apps, but SilverBullet
  loads scripts, not builds. Rejected; a package of the same elements may
  follow.
- **A fleet-origin popup to confirm risky Allows:** a second barrier host
  scripts cannot drive. Rejected by the owner for UX; the residual in §4 is
  accepted instead.
- **A short-lived bearer token handed to the host:** works cross-site, but
  the token sits in host JS, readable and replayable after the page closes.
  The cookie is the narrower exposure.

## Consequences

- Members get the agent where they work. The console stays admin-only.
- AgentService becomes reachable from allowlisted origins; every other core
  path stays same-origin.
- `gate.go`'s "never allowing CORS" comment and the `/embed/*` notes in
  `gate.go`, `headers.go`, ADR-0066 §2 and ADR-0069 §4 change with the code
  that implements this.
- Two UIs render transcripts: the console (React) and the islands (Svelte).
  Transcript parsing is shared code so they disagree on layout, not meaning.
- Hosts are `*.bnei.dev` only. A host on another site needs third-party
  cookies or tokens, and its own ADR.
- A working island needs ADR-0066's A2 (threads, messages, transcript) and
  A3 (decisions) live.

## Out of scope

- Hosts outside `bnei.dev`.
- The hq DecisionDock island (a cross-session decision queue ADR-0066 does
  not define). It will ship from the same bundle.
- A published npm package.
- Mobile layouts beyond what the host gives the element.
