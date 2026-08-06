// Exercises streamHumanMessages against a real local HTTP server (Bun.serve),
// not a mock — this is transport-layer reconnect/cursor-resume behavior,
// exactly the kind of thing a mocked fetch would be too easy to get
// "passing" without actually proving. Confirmed live in prod: a dropped
// SSE connection used to end this feed for the rest of a task's life, with
// nothing left to ever deliver Approve/Discuss/Abort again (no reconnect
// loop anywhere in the old implementation). See sidecar/internal/localapi's
// humanMessagesHandler for the other half of this fix (sinceSeq query
// param + keep-alive pings).
import { test, expect, beforeEach, afterEach } from "bun:test";

type Handler = (req: Request) => Response | Promise<Response>;

let handler: Handler = () => new Response("not configured", { status: 500 });
const receivedSinceSeqs: string[] = [];

// Bind and connect via the literal IPv4 loopback address, not "localhost"
// — confirmed live in CI (GitHub Actions' Linux runners): "localhost"
// there resolves to ::1 first, Bun.serve's default hostname only listens
// on 0.0.0.0/IPv4, and fetch() doesn't race both like a browser's Happy
// Eyeballs, so every request just hangs until the test's own 10s timeout.
// Passed clean locally on macOS the whole time, which resolves "localhost"
// straight to 127.0.0.1 — worth remembering next time a Bun.serve-backed
// test "works on my machine" but times out only in CI.
const server = Bun.serve({
  port: 0,
  hostname: "127.0.0.1",
  fetch(req) {
    const url = new URL(req.url);
    receivedSinceSeqs.push(url.searchParams.get("sinceSeq") ?? "");
    return handler(req);
  },
});
process.env.SIDECAR_API_ADDR = `127.0.0.1:${server.port}`;

const { streamHumanMessages } = await import("./sidecarClient.js");

function sseResponse(lines: string[]): Response {
  const body = lines.map((l) => `${l}\n\n`).join("");
  return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
}

function dataLine(entry: { seq: number; from: string; text: string; type?: string }): string {
  return `data: ${JSON.stringify(entry)}`;
}

beforeEach(() => {
  receivedSinceSeqs.length = 0;
  handler = () => new Response("not configured", { status: 500 });
});

afterEach(() => {
  handler = () => new Response("not configured", { status: 500 });
});

test("reconnects after the server ends the stream cleanly, resuming from the last delivered seq", async () => {
  let call = 0;
  handler = () => {
    call++;
    if (call === 1) return sseResponse([dataLine({ seq: 1, from: "human", text: "first", type: "discussion" })]);
    return sseResponse([dataLine({ seq: 2, from: "human", text: "second", type: "discussion" })]);
  };

  const entries: { seq: number; text: string }[] = [];
  const abortController = new AbortController();
  const done = streamHumanMessages((entry) => {
    entries.push({ seq: entry.seq, text: entry.text });
    if (entries.length === 2) abortController.abort();
  }, abortController.signal);

  await done;

  expect(entries).toEqual([
    { seq: 1, text: "first" },
    { seq: 2, text: "second" },
  ]);
  expect(receivedSinceSeqs[0]).toBe("0");
  expect(receivedSinceSeqs[1]).toBe("1"); // resumed from the last delivered seq, not replayed from 0
}, 10000);

test("keep-alive comment lines are ignored, not mistaken for a message", async () => {
  handler = () => sseResponse([": keep-alive", dataLine({ seq: 1, from: "human", text: "real message" })]);

  const entries: { seq: number; text: string }[] = [];
  const abortController = new AbortController();
  const done = streamHumanMessages((entry) => {
    entries.push({ seq: entry.seq, text: entry.text });
    abortController.abort();
  }, abortController.signal);

  await done;
  expect(entries).toEqual([{ seq: 1, text: "real message" }]);
}, 10000);

test("reconnects after a non-2xx response instead of giving up", async () => {
  let call = 0;
  handler = () => {
    call++;
    if (call === 1) return new Response("boom", { status: 500 });
    return sseResponse([dataLine({ seq: 1, from: "human", text: "recovered" })]);
  };

  const entries: { seq: number; text: string }[] = [];
  const abortController = new AbortController();
  const done = streamHumanMessages((entry) => {
    entries.push({ seq: entry.seq, text: entry.text });
    abortController.abort();
  }, abortController.signal);

  await done;
  expect(call).toBe(2);
  expect(entries).toEqual([{ seq: 1, text: "recovered" }]);
}, 10000);

test("stops reconnecting once aborted, even mid-backoff", async () => {
  let call = 0;
  handler = () => {
    call++;
    return new Response("boom", { status: 500 }); // always fails, would reconnect forever if not aborted
  };

  const abortController = new AbortController();
  const done = streamHumanMessages(() => {}, abortController.signal);
  await Bun.sleep(50); // let the first attempt happen and start backing off
  abortController.abort();
  await done;

  const callsAtAbort = call;
  await Bun.sleep(3000); // well past RECONNECT_DELAY_MS — no further attempts should happen
  expect(call).toBe(callsAtAbort);
}, 10000);
