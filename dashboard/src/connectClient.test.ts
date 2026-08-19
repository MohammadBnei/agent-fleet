// Drives subscribeTranscript against a fake stream, rather than grepping its
// source for expected substrings the way this file used to. The grep version
// asserted `code.toContain("if (paused)")` — it passed happily while the
// pause it was checking for did nothing: `paused` only gated the *next* loop
// iteration, so a backgrounded phone kept an open stream the OS then killed
// underneath it, which is the "connection lost" people actually saw.
//
// The fake arrives through subscribeTranscript's own `openStream` parameter
// rather than `mock.module`. Mocking the module passed locally and silently
// did not apply on CI's bun, where the "test" then exercised the real
// transport against a relative URL and reported an empty call list.
import { describe, test, expect, beforeEach, afterEach } from "bun:test";
import type { TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";
import { subscribeTranscript } from "./connectClient";

type Call = { sinceSeq: bigint; signal: AbortSignal };

const mockDocument = {
  hidden: false,
  _listeners: new Map<string, Set<EventListener>>(),
  addEventListener(event: string, listener: EventListener) {
    if (!this._listeners.has(event)) this._listeners.set(event, new Set());
    this._listeners.get(event)!.add(listener);
  },
  removeEventListener(event: string, listener: EventListener) {
    this._listeners.get(event)?.delete(listener);
  },
  _setHidden(hidden: boolean) {
    this.hidden = hidden;
    this._listeners.get("visibilitychange")?.forEach((l) => l(new Event("visibilitychange")));
  },
};

const tick = (ms = 20) => new Promise((resolve) => setTimeout(resolve, ms));

describe("subscribeTranscript visibility handling", () => {
  let originalDocument: typeof document;
  let calls: Call[];
  let queued: Partial<TranscriptEntry>[];

  // Yields whatever is queued for this attempt, then stays open exactly like
  // a live stream with nothing new to say — until somebody aborts it.
  const openStream = (req: { sessionId: string; sinceSeq: bigint }, opts: { signal: AbortSignal }) => {
    calls.push({ sinceSeq: req.sinceSeq, signal: opts.signal });
    const toYield = queued;
    queued = [];
    return (async function* () {
      for (const entry of toYield) yield entry as TranscriptEntry;
      await new Promise((_resolve, reject) => {
        opts.signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
      });
    })();
  };

  beforeEach(() => {
    originalDocument = global.document;
    // @ts-expect-error — a stand-in with only the members this uses
    global.document = mockDocument;
    mockDocument.hidden = false;
    mockDocument._listeners.clear();
    calls = [];
    queued = [];
  });

  afterEach(() => {
    global.document = originalDocument;
  });

  test("aborts the in-flight stream when the page is hidden, and resumes from the cursor", async () => {
    queued = [{ seq: 7n } as TranscriptEntry];
    const seen: bigint[] = [];
    const unsubscribe = subscribeTranscript("session-1", 0n, (entry) => seen.push(entry.seq), openStream);

    await tick();
    expect(calls).toHaveLength(1);
    expect(calls[0].sinceSeq).toBe(0n);
    expect(seen).toEqual([7n]);
    expect(calls[0].signal.aborted).toBe(false);

    mockDocument._setHidden(true);
    await tick();
    // The open connection is dropped now, not left for the OS to kill.
    expect(calls[0].signal.aborted).toBe(true);
    // ...and nothing is retried while still hidden.
    await tick(1200);
    expect(calls).toHaveLength(1);

    mockDocument._setHidden(false);
    await tick(300);
    expect(calls).toHaveLength(2);
    // Resumed one past the entry already delivered — no gap, no replay.
    expect(calls[1].sinceSeq).toBe(8n);

    unsubscribe();
  }, 10000);

  // core ends StreamTranscript itself every streamMaxAge (90s) so Cloudflare
  // never sees 100s of origin silence and 524s the connection — that timeout is
  // the only reason fleet.bnei.dev is still DNS-only rather than proxied.
  //
  // The server-side change is three lines and ships no client change at all,
  // which is only true because THIS loop treats a clean end exactly like a
  // dropped one: it resubscribes from its own cursor. Pinned here because the
  // whole design rests on it, and because `for await` completing normally is
  // easy to mistake for "the subscription is over".
  //
  // If this ever stops holding, the answer is still not a keepalive frame on
  // the stream: `seq` starts at 0 (Append assigns COALESCE(MAX(seq), -1) + 1),
  // so there is no unused sentinel, and any tab running older JS across a
  // deploy would take the sentinel as real and rewind its cursor.
  test("resubscribes from the cursor when the server ends the stream cleanly", async () => {
    // Ends after yielding — a clean return, not a throw. That is what a server
    // hitting streamMaxAge looks like from here.
    const cleanlyEnding = (req: { sessionId: string; sinceSeq: bigint }, opts: { signal: AbortSignal }) => {
      calls.push({ sinceSeq: req.sinceSeq, signal: opts.signal });
      const toYield = queued;
      queued = [];
      return (async function* () {
        for (const entry of toYield) yield entry as TranscriptEntry;
      })();
    };

    queued = [{ seq: 4n } as TranscriptEntry];
    const seen: bigint[] = [];
    const unsubscribe = subscribeTranscript("session-3", 0n, (entry) => seen.push(entry.seq), cleanlyEnding);

    await tick();
    expect(calls).toHaveLength(1);
    expect(seen).toEqual([4n]);

    // No abort, no error — the stream simply finished. The loop must come back.
    await tick(1400);
    expect(calls.length).toBeGreaterThanOrEqual(2);
    // Resumed one past what was already delivered: no gap, and no replay of
    // entries the feed has appended once already.
    expect(calls[1].sinceSeq).toBe(5n);

    unsubscribe();
  }, 10000);

  test("unsubscribe aborts the live attempt and unregisters the listener", async () => {
    const unsubscribe = subscribeTranscript("session-2", 3n, () => {}, openStream);
    await tick();
    expect(calls).toHaveLength(1);

    unsubscribe();
    await tick();
    expect(calls[0].signal.aborted).toBe(true);
    expect(mockDocument._listeners.get("visibilitychange")?.size ?? 0).toBe(0);

    // A hide/show cycle after teardown must not resurrect the stream.
    mockDocument._setHidden(true);
    mockDocument._setHidden(false);
    await tick(1200);
    expect(calls).toHaveLength(1);
  }, 10000);
});
