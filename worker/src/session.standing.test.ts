// docs/adr/0084 §4: a standing character runs one SDK session per thread, one thread at a time. The fake SDK
// holds each turn's result until the test releases it, so what is running, what waits and what is consumed
// can be read between the two. Mocks are registered before ./session.js is imported, and this file mocks
// both modules for itself only (bun test --isolate).

import { beforeEach, expect, mock, test } from "bun:test";

type Entry = {
  seq: number;
  from: string;
  text: string;
  type?: string;
  replyTo?: number;
  threadKey?: string;
};

const pushed: {
  from: string;
  text: string;
  type?: string;
  key?: string;
  thread?: string;
}[] = [];
const turns: { thread: string; seq: number }[] = [];
const savedIds: { id: string; thread?: string }[] = [];
let handler: ((e: Entry) => void | Promise<void>) | null = null;
let threadSessionIds: Record<string, string> | undefined;
let nextSeq = 1;

mock.module("./sidecarClient.js", () => ({
  pushMessage: mock(
    async (
      from: string,
      text: string,
      type?: string,
      _replyTo?: number,
      key?: string,
      thread?: string,
    ) => {
      pushed.push({ from, text, type, key, thread });
      return nextSeq++;
    },
  ),
  saveSessionId: mock(async (id: string, _model: string, thread?: string) => {
    savedIds.push({ id, thread });
  }),
  setTurn: mock(async (thread: string, seq: number) => {
    turns.push({ thread, seq });
  }),
  savePermissionMode: mock(async () => {}),
  exportSpans: mock(async () => {}),
  consumeGrant: mock(async () => ({ granted: false, decisionId: "" })),
  getSession: mock(async () => ({
    description: "character",
    permissionMode: "auto",
    decisionWaitMs: 60_000,
    agentSpec: {
      toolsAllow: [],
      toolsDeny: [],
      sidecarTools: [],
      modelAllow: [],
      thinkingDefault: "",
      modeAllow: ["auto", "default"],
      systemPromptAppend: "",
      maxTurns: 0,
      lifecycle: "standing",
    },
    agentSpecSha: "sha",
    threadSessionIds,
  })),
  streamHumanMessages: mock(
    async (
      onEntry: typeof handler,
      signal: AbortSignal,
      _start?: number,
      _sha?: string,
    ) => {
      handler = onEntry;
      await new Promise<void>((resolve) =>
        signal.addEventListener("abort", () => resolve()),
      );
    },
  ),
}));

// One fake SDK session per query(). Each input it pulls is a turn: it records it, then waits for the test to
// release that session's turn before yielding the result.
type Sdk = {
  options: Record<string, unknown>;
  inputs: string[];
  release: () => void;
  interrupts: number;
};
const sdks: Sdk[] = [];

mock.module("@anthropic-ai/claude-agent-sdk", () => ({
  query: ({
    prompt,
    options,
  }: {
    prompt: AsyncIterable<{ message: { content: string } }>;
    options: Record<string, unknown>;
  }) => {
    const iterator = prompt[Symbol.asyncIterator]();
    const abort = options.abortController as AbortController;
    const sdk: Sdk = { options, inputs: [], release: () => {}, interrupts: 0 };
    sdks.push(sdk);
    const id = `sdk-${sdks.length}`;
    async function* generate() {
      yield { type: "system", subtype: "init", session_id: id, model: "m" };
      for (;;) {
        let next: IteratorResult<{ message: { content: string } }>;
        try {
          next = await Promise.race([
            iterator.next(),
            new Promise<never>((_, reject) => {
              if (abort.signal.aborted) reject(new Error("aborted"));
              abort.signal.addEventListener("abort", () =>
                reject(new Error("aborted")),
              );
            }),
          ]);
        } catch {
          return;
        }
        if (next.done) return;
        sdk.inputs.push(next.value.message.content);
        // The turn ends when the test releases it, or the session is aborted, as a real one is.
        const aborted = await new Promise<boolean>((resolve) => {
          sdk.release = () => resolve(false);
          if (abort.signal.aborted) resolve(true);
          abort.signal.addEventListener("abort", () => resolve(true));
        });
        if (aborted) return;
        yield {
          type: "result",
          subtype: "success",
          num_turns: 1,
          total_cost_usd: 0,
          queued_turn_count: 0,
        };
      }
    }
    return Object.assign(generate(), {
      interrupt: mock(async () => {
        sdk.interrupts++;
      }),
      setPermissionMode: mock(async () => {}),
      getContextUsage: mock(async () => ({ memoryFiles: [], skills: {} })),
    });
  },
}));

for (const key of ["RESUME_SESSION_ID", "RESUME_FROM_SEQ", "CLAUDE_MODEL"])
  delete process.env[key];
process.env.SESSION_ID = "S";
process.env.FLEET_DEFINITION = "dev-notes";

const { runSession } = await import("./session.js");

beforeEach(() => {
  pushed.length = 0;
  turns.length = 0;
  savedIds.length = 0;
  sdks.length = 0;
  handler = null;
  threadSessionIds = undefined;
  nextSeq = 1;
  delete process.env.TURN_DEADLINE_MS;
});

const msg = (seq: number, thread: string, text = `m${seq}`): Entry => ({
  seq,
  from: "human",
  text,
  type: "discussion",
  threadKey: thread,
});

const results = () =>
  pushed
    .filter((p) => p.type === "result")
    .map((p) => ({ ...JSON.parse(p.text), key: p.key, thread: p.thread }));

// Waits until cond holds, so no test sleeps a fixed time on the happy path.
async function until(cond: () => boolean): Promise<void> {
  for (let i = 0; i < 200 && !cond(); i++) await Bun.sleep(5);
  expect(cond()).toBe(true);
}

async function stop(run: Promise<{ aborted: boolean }>): Promise<void> {
  await handler?.({ seq: 999, from: "human", text: "stop", type: "abort" });
  expect((await run).aborted).toBe(true);
}

test("one thread runs at once: the other waits at its head until the running turn ends", async () => {
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.(msg(11, "b"));
  await until(() => sdks[0]?.inputs.length === 1);
  // b has its own SDK session, but its message has not been handed to it.
  expect(sdks.length).toBe(2);
  expect(sdks[1].inputs).toEqual([]);

  sdks[0].release();
  await until(() => sdks[1].inputs.length === 1);
  expect(sdks[1].inputs[0]).toContain("m11");
  sdks[1].release();
  await until(() => results().length === 2);
  await stop(run);
});

test("a result carries its own thread's consumedSeq, key and thread", async () => {
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.(msg(11, "b"));
  await until(() => sdks[0]?.inputs.length === 1);
  sdks[0].release();
  await until(() => sdks[1]?.inputs.length === 1);
  sdks[1].release();
  await until(() => results().length === 2);
  const [ra, rb] = results();
  expect(ra.consumedSeq).toBe(10);
  expect(ra.thread).toBe("a");
  expect(ra.key).toBe("result:S:a:10:1");
  expect(rb.consumedSeq).toBe(11);
  expect(rb.thread).toBe("b");
  await stop(run);
});

test("a waiting message is never skipped past: an answer above it does not move the cursor over it", async () => {
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await until(() => sdks[0]?.inputs.length === 1);
  await handler?.(msg(12, "a", "second"));
  // An answer passes the running turn at once, and is taken in above the waiting message.
  await handler?.({
    seq: 15,
    from: "human",
    text: JSON.stringify({ answers: {} }),
    type: "answer",
    replyTo: 9,
    threadKey: "a",
  });
  sdks[0].release();
  await until(() => results().length === 1);
  // 12 waits and has not been taken in, so the thread is done up to 11 only.
  expect(results()[0].consumedSeq).toBe(11);
  await stop(run);
});

test("the turn deadline aborts the turn and the next entry runs", async () => {
  process.env.TURN_DEADLINE_MS = "40";
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.(msg(11, "b"));
  await until(() => sdks[0]?.inputs.length === 1);
  // a never results; its deadline interrupts it and b is delivered.
  await until(() => sdks[1]?.inputs.length === 1);
  expect(sdks[0].interrupts).toBe(1);
  expect(pushed.some((p) => p.type === "system" && p.thread === "a")).toBe(
    true,
  );
  await stop(run);
});

test("a late result of a turn the deadline cut changes nothing for the thread that runs now", async () => {
  process.env.TURN_DEADLINE_MS = "60";
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.(msg(11, "b"));
  await until(() => sdks[1]?.inputs.length === 1);
  // The deadline already wrote a's result so its cursor moved, and b runs.
  expect(results().map((r) => [r.thread, r.subtype])).toEqual([
    ["a", "deadline"],
  ]);
  const before = turns.length;
  sdks[0].release();
  await Bun.sleep(30);
  // a's late result neither clears b's turn nor writes a second result.
  expect(turns.slice(before)).toEqual([]);
  expect(results().length).toBe(1);
  await stop(run);
});

test("an answer for a thread that is not running waits its turn instead of starting a second one", async () => {
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await until(() => sdks[0]?.inputs.length === 1);
  await handler?.({
    seq: 11,
    from: "human",
    text: JSON.stringify({ answers: {} }),
    type: "answer",
    replyTo: 5,
    threadKey: "b",
  });
  expect(sdks[1]?.inputs.length ?? 0).toBe(0);
  expect(turns).toEqual([{ thread: "a", seq: 10 }]);
  sdks[0].release();
  await until(() => sdks[1]?.inputs.length === 1);
  expect(turns.at(-1)).toEqual({ thread: "b", seq: 11 });
  await stop(run);
});

test("a closed thread ends only its own SDK session, and a later entry resumes it (parked)", async () => {
  threadSessionIds = { b: "old-b" };
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.(msg(11, "b"));
  expect(sdks[1].options.resume).toBe("old-b");
  await handler?.({
    seq: 12,
    from: "human",
    text: "closed",
    type: "abort",
    threadKey: "b",
  });
  expect(sdks[1].options.abortController as AbortController).toBeDefined();
  expect(
    (sdks[1].options.abortController as AbortController).signal.aborted,
  ).toBe(true);
  expect(
    (sdks[0].options.abortController as AbortController).signal.aborted,
  ).toBe(false);
  await handler?.(msg(13, "b", "again"));
  expect(sdks.length).toBe(3);
  // The id this pod's SDK session ended with (the fake reports its own), not the one read at boot.
  expect(sdks[2].options.resume).toBe("sdk-2");
  await stop(run);
});

test("closing a thread this pod never started leaves its kept SDK session to resume", async () => {
  threadSessionIds = { b: "old-b" };
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.({
    seq: 11,
    from: "human",
    text: "closed",
    type: "abort",
    threadKey: "b",
  });
  expect(sdks.length).toBe(1);
  await handler?.(msg(12, "b", "again"));
  expect(sdks.length).toBe(2);
  expect(sdks[1].options.resume).toBe("old-b");
  await stop(run);
});

test("closing a waiting thread leaves the running thread's parked permission alone", async () => {
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.(msg(11, "b"));
  await until(() => sdks[0]?.inputs.length === 1);
  let settled: unknown;
  const can = sdks[0].options.canUseTool as (
    tool: string,
    input: Record<string, unknown>,
    o: { signal: AbortSignal; toolUseID: string },
  ) => Promise<unknown>;
  void can(
    "Bash",
    { command: "ls" },
    {
      signal: new AbortController().signal,
      toolUseID: "t1",
    },
  ).then((r) => (settled = r));
  await until(() => pushed.some((p) => p.type === "permission_request"));
  await handler?.({
    seq: 12,
    from: "human",
    text: "closed",
    type: "abort",
    threadKey: "b",
  });
  await Bun.sleep(20);
  expect(settled).toBeUndefined();
  await stop(run);
  expect(settled).toBeDefined();
});

test("Stop in main ends every thread and the run", async () => {
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "a"));
  await handler?.(msg(11, "b"));
  await stop(run);
  for (const s of sdks)
    expect((s.options.abortController as AbortController).signal.aborted).toBe(
      true,
    );
});

test("each thread saves its own SDK session id under its key, main under none", async () => {
  const run = runSession();
  await until(() => handler !== null);
  await handler?.(msg(10, "main"));
  await handler?.(msg(11, "b"));
  await until(() => savedIds.length === 2);
  expect(savedIds.find((s) => s.id === "sdk-1")?.thread).toBeUndefined();
  expect(savedIds.find((s) => s.id === "sdk-2")?.thread).toBe("b");
  await stop(run);
});
