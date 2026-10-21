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
- It calls **`AgentService` only.** A lint import guard keeps `islands/` off
  `DashboardService` and off `dashboard/src` components.

### 2. Host API

- **Attributes:** `definition` (required), `context` (the host's current
  page path, passed to `CreateThread` as a labelled reference, validated
  server-side as a workspace path), `thread` (open a given thread).
- **Events** (DOM `CustomEvent`, bubbling, composed): `fleet-status`
  (`loading | working | waiting | idle | error`), `fleet-message` (a new
  agent entry, its id and kind only).
- **Theme:** the element reads the host's CSS custom properties
  (`--fleet-*`, falling back to sensible defaults) through its Shadow DOM.
- Nothing else. The host cannot call methods that act on the session; the
  element acts only on its own user's clicks.

### 3. Auth: the fleet cookie, same-site hosts, CORS on AgentService only

- **Same-site hosts only.** `opus.bnei.dev` and `fleet.bnei.dev` are the same
  site (`bnei.dev` is not a public suffix), so the `__Host-` `SameSite=Lax`
  session cookie is sent on a credentialed `fetch` and is not a third-party
  cookie that browsers block. The definitions validator already limits
  `embed.origins` to exact `https://` origins on `bnei.dev` or `*.bnei.dev`.
- **CORS (amends ADR-0066 §1).** Core answers CORS on the `AgentService`
  path prefix only, and only for an `Origin` that some valid definition lists
  in `embed.origins`: the exact origin echoed, `Access-Control-Allow-
  Credentials: true`, `Vary: Origin`, and the preflight allowing the Connect
  and CSRF headers. Every other path, `DashboardService` and `/auth/*`
  included, still sends no CORS header. The per-method authz (ADR-0066 §4)
  then decides per definition: a call naming a definition whose
  `embed.origins` does not list the request's `Origin` is refused, so an
  origin allowed for one definition cannot drive another.
- **Static bundles.** `/islands/*` joins the gate's exempt paths as the
  first prefix: immutable, hashed JS and CSS with no data, served with
  `Access-Control-Allow-Origin: *` (module scripts are fetched in CORS mode)
  and long cache headers.
- **Login.** On `Unauthenticated` the element shows "Sign in", which opens
  `fleet.bnei.dev/auth/login?popup=1` top-level in a popup. The callback
  sets the cookie and lands on a page that closes itself. The element does
  not rely on `window.opener`: it retries the call when its window regains
  focus. Core never sets `Cross-Origin-Opener-Policy: same-origin` on
  `/auth/*`.
- **No iframe, so no `/embed/*`.** `frame-ancestors 'none'` (ADR-0069)
  stays on every path; nothing in the fleet is framed.

### 4. The boundary is the host origin's scripts

**The cookie authenticates the browser, not the gesture. Any script running
on an allowlisted origin acts as the signed-in member while the page is
open.** `HttpOnly` stops a script from reading the cookie; it does not stop
one from sending it, and the CSRF header does not help, since a script on an
allowed origin can set it too. Shadow DOM and `event.isTrusted` are not
boundaries either: the script calls the API, not the element.

- All decisions are inline, the riskiest included. There is no fleet-origin
  confirmation step.
- On a host the agent can write to (the Opus space is the agent's own
  workspace), the control is the **script-write ask** (P5 spike 5): any write
  of `.js`/`.mjs`, a `space-lua`/`space-script` fence, `${`, `js.import`,
  `js.window` or a `Library/` install asks a human, even in `auto`.
- **Accepted residual:** one approved script, or any XSS on an allowlisted
  host, can act as every member who opens that page, including approving the
  agent's own permission requests and plans, until the script is removed.
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
