<svelte:options
  customElement={{
    tag: "fleet-agent",
    props: {
      definition: { type: "String" },
      context: { type: "String" },
      thread: { type: "String" },
    },
  }}
/>

<script lang="ts">
  import { onMount } from "svelte";
  import type { Thread, Entry } from "../../dashboard/src/gen/agentfleet/v1/agent_pb";
  import { Code, ConnectError } from "@connectrpc/connect";
  import { agentClient, errorText, fleetOrigin, isUnauthenticated } from "./api";
  import { conversation, decisions } from "./feed";
  import { createRenderer } from "./render";
  import { drawDiagrams } from "./diagram";
  import tokens from "./tokens.css?inline";
  import styles from "./styles.css?inline";

  let { definition = "", context = "", thread = "" }: { definition?: string; context?: string; thread?: string } = $props();

  const el = $host();
  const api = agentClient();
  const render = createRenderer(window);

  let phase = $state<"loading" | "signed-out" | "ready" | "error">("loading");
  let error = $state("");
  let threads = $state<Thread[]>([]);
  let current = $state<Thread | undefined>();
  let entries = $state<Entry[]>([]);
  let draft = $state("");
  let busy = $state(false);
  let answers = $state<Record<string, string>>({});
  let reconnecting = $state(false);
  let stream: AbortController | undefined;
  // Bumped by every load/select/unmount. Work started under an older value
  // writes nothing: a slow response for thread A never lands in thread B.
  let gen = 0;
  let seen = new Set<bigint>();

  const items = $derived(conversation(entries));
  const open = $derived(decisions(entries));
  const live = $derived(open.length ? "waiting" : (current?.status?.state ?? "idle"));

  // One event per change, for the host (§2).
  let lastState = "";
  $effect(() => {
    if (live === lastState) return;
    lastState = live;
    el.dispatchEvent(new CustomEvent("fleet-status", { detail: { state: live }, bubbles: true, composed: true }));
  });

  // Fonts cannot be declared inside a shadow root; add them to the page once.
  function ensureFonts() {
    if (document.querySelector("link[data-fleet-fonts]")) return;
    const link = document.createElement("link");
    link.rel = "stylesheet";
    link.dataset.fleetFonts = "";
    link.href = "https://fonts.googleapis.com/css2?family=Readex+Pro:wght@300..700&family=JetBrains+Mono:wght@400;500&display=swap";
    document.head.append(link);
  }

  async function guard(fn: () => Promise<void>) {
    try {
      await fn();
    } catch (err) {
      if (isUnauthenticated(err)) {
        phase = "signed-out";
      } else {
        error = errorText(err);
        if (phase === "loading") phase = "error";
      }
    }
  }

  async function load() {
    const my = ++gen;
    stream?.abort();
    phase = "loading";
    error = "";
    await guard(async () => {
      const res = await api.listThreads({ definition });
      if (my !== gen) return;
      threads = res.threads;
      phase = "ready";
      const pick = threads.find((t) => t.id === thread) ?? threads[0];
      if (pick) await select(pick);
    });
  }

  async function select(t: Thread) {
    const my = ++gen;
    stream?.abort();
    current = t;
    entries = [];
    seen = new Set();
    answers = {};
    error = "";
    await guard(async () => {
      const res = await api.getTranscript({ threadId: t.id });
      if (my !== gen) return;
      entries = res.entries;
      seen = new Set(res.entries.map((e) => e.seq));
      follow(t.id, res.nextSeq, my);
    });
  }

  // Streams new entries from since_seq (inclusive, like core's ReadSince); on
  // a drop it backs off and resumes after the last seq it saw. NotFound and
  // PermissionDenied are final: the thread is gone or no longer visible.
  async function follow(threadId: string, since: bigint, my: number) {
    const ctl = new AbortController();
    stream = ctl;
    let delay = 1000;
    while (!ctl.signal.aborted && my === gen) {
      try {
        for await (const res of api.streamTranscript({ threadId, sinceSeq: since }, { signal: ctl.signal })) {
          if (my !== gen) return;
          reconnecting = false;
          const e = res.entry;
          if (!e || seen.has(e.seq)) continue;
          seen.add(e.seq);
          entries = [...entries, e];
          since = e.seq + 1n;
          delay = 1000;
          if (e.from === "agent") {
            el.dispatchEvent(new CustomEvent("fleet-message", { detail: { seq: String(e.seq), type: e.type }, bubbles: true, composed: true }));
          }
        }
      } catch (err) {
        if (ctl.signal.aborted || my !== gen) return;
        if (isUnauthenticated(err)) {
          phase = "signed-out";
          return;
        }
        const code = ConnectError.from(err).code;
        if (code === Code.NotFound || code === Code.PermissionDenied) {
          error = "This thread is no longer available.";
          return;
        }
      }
      if (my !== gen) return;
      reconnecting = true;
      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 2, 30000);
    }
  }

  async function newThread(visibility: "shared" | "private") {
    await guard(async () => {
      const title = context ? `About ${context}` : "";
      const res = await api.createThread({ definition, visibility, title });
      if (res.thread) {
        threads = [res.thread, ...threads];
        await select(res.thread);
      }
    });
  }

  async function act(fn: () => Promise<unknown>) {
    busy = true;
    error = "";
    await guard(async () => {
      await fn();
    });
    busy = false;
  }

  function send() {
    const text = draft.trim();
    if (!text || !current) return;
    const threadId = current.id;
    act(async () => {
      await api.postMessage({ threadId, text });
      draft = "";
    });
  }

  const answer = (seq: bigint) =>
    act(() => api.answerQuestion({ threadId: current!.id, seq, answersJson: JSON.stringify({ answers }) }));
  const permit = (seq: bigint, behavior: "allow" | "deny") =>
    act(() => api.respondToPermission({ threadId: current!.id, seq, behavior }));
  const approve = (seq: bigint) => act(() => api.approvePlan({ threadId: current!.id, seq }));

  // Login runs top-level on the fleet (authentik refuses to be framed). No
  // reliance on window.opener: the island retries when its window regains focus.
  function signIn() {
    const url = `${fleetOrigin}/auth/login?popup=1`;
    const w = window.open(url, "fleet-login", "popup,width=480,height=680");
    if (!w) window.open(url, "_blank", "noopener");
  }

  onMount(() => {
    // Styles live in the shadow root: Ukubi tokens first, then the island's.
    // Once: the shadow root outlives a disconnect and remount.
    if (!el.shadowRoot?.querySelector("style[data-fleet]")) {
      const style = document.createElement("style");
      style.dataset.fleet = "";
      style.textContent = tokens + styles;
      el.shadowRoot?.prepend(style);
    }
    ensureFonts();
    load();
    const onFocus = () => {
      if (phase === "signed-out") load();
    };
    window.addEventListener("focus", onFocus);
    return () => {
      window.removeEventListener("focus", onFocus);
      gen++;
      stream?.abort();
    };
  });

  const label = (t: Thread) => t.title || `Thread ${t.id.slice(0, 6)}`;
  const statusWord: Record<string, string> = {
    loading: "Starting",
    working: "Working",
    waiting: "Needs you",
    idle: "Idle",
    error: "Error",
  };
  const toolLine = (input: unknown) => {
    const i = input as { command?: unknown; file_path?: unknown } | undefined;
    return String(i?.command ?? i?.file_path ?? "");
  };
</script>

<section class="island" aria-label="Agent">
  {#if phase === "loading"}
    <p class="muted pad">Loading…</p>
  {:else if phase === "signed-out"}
    <div class="pad stack">
      <p>Sign in to talk to the agent.</p>
      <button class="primary chamfer" onclick={signIn}>Sign in</button>
      <button class="secondary" onclick={load}>I signed in, retry</button>
    </div>
  {:else if phase === "error"}
    <div class="pad stack">
      <p class="danger">Could not reach the agent: {error}</p>
      <button class="secondary" onclick={load}>Retry</button>
    </div>
  {:else}
    <header class="bar">
      <select aria-label="Thread" onchange={(e) => { const t = threads.find((x) => x.id === e.currentTarget.value); if (t) select(t); }}>
        {#each threads as t (t.id)}
          <option value={t.id} selected={t.id === current?.id}>{label(t)} · {t.visibility}</option>
        {/each}
        {#if !threads.length}<option value="">No threads yet</option>{/if}
      </select>
      <button class="secondary" onclick={() => newThread("shared")}>New shared</button>
      <button class="secondary" onclick={() => newThread("private")}>New private</button>
      {#if current}
        <span class="status" data-state={live}>
          {#if live === "waiting"}<span class="led" aria-hidden="true"></span>{/if}
          {reconnecting ? "Reconnecting" : (statusWord[live] ?? live)}{#if current.status?.code} · {current.status.code}{/if}
        </span>
      {/if}
    </header>

    {#if error}<p class="danger pad-x">{error}</p>{/if}

    {#if !current}
      <div class="pad stack">
        <p>No conversation yet. Start one{context ? ` about ${context}` : ""}.</p>
        <button class="primary chamfer" onclick={() => newThread("shared")}>Start a shared thread</button>
      </div>
    {:else}
      <ol class="feed" use:drawDiagrams={{ items, svg: render.svg }}>
        {#each items as m (m.seq)}
          {#if m.kind === "message"}
            <li class:human={m.human} class="msg">
              <span class="who">{m.author}</span>
              {#if m.human}
                <p class="text">{m.text}</p>
              {:else}
                <div class="md">{@html render.markdown(m.text)}</div>
              {/if}
            </li>
          {/if}
        {/each}
        {#if !items.length}<li class="muted">Nothing said yet.</li>{/if}
      </ol>

      {#each open as d (d.seq)}
        <div class="decision chamfer-md" role="group" aria-label="Needs you">
          {#if d.kind === "question"}
            {#each d.questions as q (q.question)}
              <p class="title">{q.question}</p>
              <div class="options">
                {#each q.options as o (o.label)}
                  <button class:picked={answers[q.question] === o.label} class="secondary" onclick={() => (answers = { ...answers, [q.question]: o.label })} title={o.description}>{o.label}</button>
                {/each}
              </div>
            {/each}
            <button class="primary chamfer" disabled={busy || d.questions.some((q) => !answers[q.question])} onclick={() => answer(d.seq)}>Answer</button>
          {:else if d.kind === "plan"}
            <p class="title">The agent proposes a plan</p>
            <div class="md plan">{@html render.markdown(d.plan)}</div>
            <div class="options">
              <button class="primary chamfer" disabled={busy} onclick={() => approve(d.seq)}>Approve plan</button>
              <button class="secondary" disabled={busy} onclick={() => permit(d.seq, "deny")}>Ask for changes</button>
            </div>
          {:else if d.kind === "permission"}
            <p class="title">Allow {d.tool}?</p>
            {#if toolLine(d.input)}<pre class="code">{toolLine(d.input)}</pre>{/if}
            <div class="options">
              <button class="primary chamfer" disabled={busy} onclick={() => permit(d.seq, "allow")}>Allow</button>
              <button class="secondary danger-text" disabled={busy} onclick={() => permit(d.seq, "deny")}>Deny</button>
            </div>
          {/if}
        </div>
      {/each}

      <form class="composer" onsubmit={(e) => { e.preventDefault(); send(); }}>
        <textarea
          aria-label="Message"
          placeholder="Write to the agent"
          rows="2"
          bind:value={draft}
          onkeydown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }}
        ></textarea>
        <button class="primary chamfer" type="submit" disabled={busy || !draft.trim()}>Send</button>
      </form>
    {/if}
  {/if}
</section>
