// Drives subscribeTranscript against a fake Connect client, rather than
// grepping its source for expected substrings the way this file used to.
// The grep version asserted `code.toContain("if (paused)")` — it passed
// happily while the pause it was checking for did nothing: `paused` only
// gated the *next* loop iteration, so a backgrounded phone kept an open
// stream the OS then killed underneath it, which is the "connection lost"
// people actually saw.
import { describe, test, expect, beforeEach, afterEach, mock } from "bun:test";
import type { TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

type Call = { sinceSeq: bigint; signal: AbortSignal };
const calls: Call[] = [];
// Entries the next stream attempt should yield before it goes quiet.
let queued: Partial<TranscriptEntry>[] = [];

mock.module("@connectrpc/connect-web", () => ({
  createConnectTransport: () => ({}),
}));
mock.module("@connectrpc/connect", () => ({
  createClient: () => ({
    streamTranscript(
      req: { taskId: string; sinceSeq: bigint },
      opts: { signal: AbortSignal },
    ): AsyncIterable<TranscriptEntry> {
      calls.push({ sinceSeq: req.sinceSeq, signal: opts.signal });
      const toYield = queued;
      queued = [];
      return (async function* () {
        for (const entry of toYield) yield entry as TranscriptEntry;
        // Then stay open, exactly like a live stream with nothing new to
        // say — until somebody aborts it.
        await new Promise((_resolve, reject) => {
          opts.signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        });
      })();
    },
  }),
}));

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
  _trigger(event: string) {
    this._listeners.get(event)?.forEach((listener) => listener(new Event(event)));
  },
  _setHidden(hidden: boolean) {
    this.hidden = hidden;
    this._trigger("visibilitychange");
  },
};

const { subscribeTranscript } = await import("./connectClient");

const tick = (ms = 20) => new Promise((resolve) => setTimeout(resolve, ms));

describe("subscribeTranscript visibility handling", () => {
  let originalDocument: typeof document;

  beforeEach(() => {
    originalDocument = global.document;
    // @ts-expect-error — a stand-in with only the two members this uses
    global.document = mockDocument;
    mockDocument.hidden = false;
    mockDocument._listeners.clear();
    calls.length = 0;
    queued = [];
  });

  afterEach(() => {
    global.document = originalDocument;
  });

  test("aborts the in-flight stream when the page is hidden, and resumes from the cursor", async () => {
    queued = [{ seq: 7n } as TranscriptEntry];
    const seen: bigint[] = [];
    const unsubscribe = subscribeTranscript("task-1", 0n, (entry) => seen.push(entry.seq));

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

  test("unsubscribe aborts the live attempt and unregisters the listener", async () => {
    const unsubscribe = subscribeTranscript("task-2", 3n, () => {});
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
