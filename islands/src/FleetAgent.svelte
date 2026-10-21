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
  import { agentClient, errorText, fleetOrigin, isAlreadyAnswered, isStale, isUnauthenticated } from "./api";
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
  // DEFINITION_CHANGED: this thread is pinned to an older definition.
  let stale = $state(false);
  let composer = $state<HTMLTextAreaElement | undefined>();
  // Sent but not yet echoed by the stream: shown at once, faded (optimistic).
  let pending = $state<{ key: number; text: string; seq?: bigint }[]>([]);
  let pendingKey = 0;
  let sending = false;
  // With no thread selected, the first message creates one with this visibility.
  let newVisibility = $state<"shared" | "private">("shared");
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
      } else if (!isAlreadyAnswered(err)) {
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
    pending = [];
    error = "";
    busy = false;
    stale = false;
    reconnecting = false;
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
          // The echo of a message this island sent replaces its pending copy.
          if (e.from === "human" && pending.length) {
            const i = pending.findIndex((p) => p.seq === e.seq || p.text === e.text);
            if (i >= 0) pending = pending.filter((_, j) => j !== i);
          }
          since = e.seq + 1n;
          delay = 1000;
          if (e.from === "agent") {
            el.dispatchEvent(new CustomEvent("fleet-message", { detail: { seq: String(e.seq), type: e.type }, bubbles: true, composed: true }));
          }
        }
      } catch (err) {
        if (ctl.signal.aborted || my !== gen) return;
        if (isUnauthenticated(err)) {
          reconnecting = false;
          phase = "signed-out";
          return;
        }
        const code = ConnectError.from(err).code;
        if (code === Code.NotFound || code === Code.PermissionDenied) {
          reconnecting = false;
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

  // "New thread": nothing is created until the first message, so empty
  // threads never pile up (they all looked alike).
  function startNew() {
    gen++;
    stream?.abort();
    current = undefined;
    entries = [];
    pending = [];
    error = "";
    stale = false;
    reconnecting = false;
    queueMicrotask(() => composer?.focus());
  }

  // One action at a time, bound to the thread it was clicked in: a reply for
  // a thread the user has left touches nothing (gen, as in select).
  async function act(fn: (threadId: string) => Promise<unknown>): Promise<boolean> {
    if (busy || !current) return false;
    const my = gen;
    const threadId = current.id;
    busy = true;
    error = "";
    try {
      await fn(threadId);
      return my === gen;
    } catch (err) {
      if (my !== gen) return false;
      if (isUnauthenticated(err)) phase = "signed-out";
      else if (isStale(err)) stale = true;
      else if (isAlreadyAnswered(err)) {
        // Another member answered first. Reload, in case the stream is down.
        if (current) await select(current);
      } else {
        error = errorText(err);
        // A refusal may mean the thread became tainted since it was listed.
        if (ConnectError.from(err).code === Code.PermissionDenied) await refreshCurrent();
      }
      return false;
    } finally {
      if (my === gen) busy = false;
    }
  }

  async function refreshCurrent() {
    const my = gen;
    try {
      const res = await api.listThreads({ definition });
      if (my !== gen) return;
      threads = res.threads;
      current = threads.find((t) => t.id === current?.id) ?? current;
    } catch {
      // Best effort: the error already shown stands.
    }
  }

  const MAX_MESSAGE = 64 * 1024;

  // Optimistic: the box clears and the message shows at once, faded until the
  // stream echoes it; on failure it comes back to the box. Nothing is disabled.
  async function send() {
    const text = draft.trim();
    if (!text || sending) return;
    if (new TextEncoder().encode(text).length > MAX_MESSAGE) {
      error = "This message is too long (64 KiB at most).";
      return;
    }
    sending = true;
    error = "";
    draft = "";
    let key = 0;
    try {
      let t = current;
      if (!t) {
        // The first line names the thread, so threads tell apart in the list.
        const title = text.split("\n")[0].slice(0, 80);
        const res = await api.createThread({ definition, visibility: newVisibility, title });
        if (!res.thread) throw new Error("no thread");
        t = res.thread;
        threads = [t, ...threads.filter((x) => x.id !== t!.id)];
        await select(t);
      }
      const my = gen;
      key = ++pendingKey;
      pending = [...pending, { key, text }];
      const res = await api.postMessage({ threadId: t.id, text });
      if (my !== gen) return;
      // Already streamed back: drop the copy now; else the stream drops it by seq.
      if (seen.has(res.seq)) pending = pending.filter((p) => p.key !== key);
      else pending = pending.map((p) => (p.key === key ? { ...p, seq: res.seq } : p));
    } catch (err) {
      pending = pending.filter((p) => p.key !== key);
      if (!draft) draft = text;
      if (isUnauthenticated(err)) phase = "signed-out";
      else if (isStale(err)) stale = true;
      else {
        error = errorText(err);
        if (ConnectError.from(err).code === Code.PermissionDenied) await refreshCurrent();
      }
    } finally {
      sending = false;
    }
  }

  async function answer(seq: bigint) {
    if (await act((threadId) => api.answerQuestion({ threadId, seq, answersJson: JSON.stringify({ answers }) }))) answers = {};
  }
  const permit = (seq: bigint, behavior: "allow" | "deny") => act((threadId) => api.respondToPermission({ threadId, seq, behavior }));
  const approve = (seq: bigint) => act((threadId) => api.approvePlan({ threadId, seq }));

  // Login runs top-level on the fleet (authentik refuses to be framed). No
  // reliance on window.opener: the island retries when its window regains focus.
  function signIn() {
    // return_to is local-only (safeReturnTo); /auth/done closes the popup.
    const url = `${fleetOrigin}/auth/login?return_to=${encodeURIComponent("/auth/done")}`;
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

  const when = (iso: string) => {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return "";
    const today = new Date().toDateString() === d.toDateString();
    return today ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : d.toLocaleDateString([], { day: "numeric", month: "short" });
  };
  const label = (t: Thread) =>
    `${t.visibility === "private" ? "Private · " : ""}${t.title || "Untitled"}${t.createdAt ? ` · ${when(t.createdAt)}` : ""}`;
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
    <p class="muted">Loading…</p>
  {:else if phase === "signed-out"}
    <div class="row">
      <p class="grow">Sign in to talk to the agent.</p>
      <button class="primary chamfer" onclick={signIn}>Sign in</button>
      <button class="secondary" onclick={load} title="Retry after signing in">Retry</button>
    </div>
  {:else if phase === "error"}
    <div class="row">
      <p class="danger grow">Could not reach the agent: {error}</p>
      <button class="secondary" onclick={load}>Retry</button>
    </div>
  {:else}
    <header class="row">
      <select
        class="grow"
        aria-label="Thread"
        onchange={(e) => {
          const v = e.currentTarget.value;
          if (v === "") startNew();
          else { const t = threads.find((x) => x.id === v); if (t) select(t); }
        }}
      >
        <option value="" selected={!current}>+ New thread</option>
        {#each threads as t (t.id)}
          <option value={t.id} selected={t.id === current?.id}>{label(t)}</option>
        {/each}
      </select>
      {#if current}
        <span class="status" data-state={live} title={reconnecting ? "Reconnecting" : (statusWord[live] ?? live)}>
          {#if live === "waiting"}<span class="led" aria-hidden="true"></span>{:else}<span class="dot" aria-hidden="true"></span>{/if}
          <span class="status-word">{reconnecting ? "Reconnecting" : (statusWord[live] ?? live)}</span>
        </span>
      {:else}
        <div class="seg" role="group" aria-label="Who sees the new thread">
          <button class:on={newVisibility === "shared"} onclick={() => (newVisibility = "shared")}>Shared</button>
          <button class:on={newVisibility === "private"} onclick={() => (newVisibility = "private")}>Private</button>
        </div>
      {/if}
    </header>

    {#if error}<p class="danger">{error}</p>{/if}
    {#if stale}
      <div class="row">
        <p class="danger grow">This agent was updated. Continue in a new thread.</p>
        <button class="primary chamfer" onclick={startNew}>New thread</button>
      </div>
    {/if}

    {#if current || pending.length}
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
        {#each pending as p (p.key)}
          <li class="msg human pending"><span class="who">You · sending</span><p class="text">{p.text}</p></li>
        {/each}
      </ol>
    {:else}
      <p class="muted">Write below to start a {newVisibility} thread{context ? ` about ${context}` : ""}.</p>
    {/if}

    {#if current?.tainted && open.length}
      <p class="muted">This thread holds untrusted input, so only an admin can answer it, from the console.</p>
    {/if}
    {#each current && !current.tainted ? open : [] as d (d.seq)}
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
            <!-- A reply resolves a pending plan; AgentService has no plan deny. -->
            <button class="secondary" disabled={busy} onclick={() => composer?.focus()}>Ask for changes</button>
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
        placeholder={current?.tainted ? "Only an admin can write in this thread" : "Write to the agent"}
        rows="1"
        disabled={current?.tainted || stale}
        bind:this={composer}
        bind:value={draft}
        onkeydown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } }}
      ></textarea>
      <button class="primary chamfer send" type="submit" disabled={current?.tainted || stale || !draft.trim()}>Send</button>
    </form>
  {/if}
</section>
