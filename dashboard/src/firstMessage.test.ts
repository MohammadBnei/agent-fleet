import { test, expect } from "bun:test";

// The queue is module state consumed by delete-before-send, so its safety is
// testable without React: whatever the effect does, the SECOND read must find
// nothing.
//
// Why it is module state and not a prop: the dialog can no longer await
// postMessage (that call boots the pod — clone, fleet-shared sync, PVC — and
// awaiting it held the modal open for all of it), so the text has to reach the
// detail view some other way. A prop plus a "did I send it?" ref is not enough:
// StrictMode double-invokes effects, and the desktop/mobile media-query switch
// unmounts one detail view to mount the other, resetting any per-component
// guard. The server cannot save us either — Append mints a fresh idempotency
// key per call, so a double send is two real messages to the agent.
const src = await Bun.file(new URL("./useSessionDetail.ts", import.meta.url)).text();

test("the first message is deleted before it is sent, not after", () => {
  const body = src.slice(src.indexOf("const queued = firstMessages.get"));
  const del = body.indexOf("firstMessages.delete");
  const send = body.indexOf("sendDiscuss(queued)");
  expect(del, "the queued message is never deleted — every remount re-sends it").toBeGreaterThan(-1);
  expect(send).toBeGreaterThan(-1);
  expect(del, "delete must come BEFORE the send, or a StrictMode double-invoke sends twice").toBeLessThan(send);
});

test("an empty first message is never queued", () => {
  const fn = src.slice(src.indexOf("export function queueFirstMessage"));
  // Creating a session with no message is a valid resting state (adr/0048 §1):
  // it must boot no pod, so an empty string must not reach postMessage.
  expect(fn.slice(0, 200)).toContain("trim()");
});

test("the dialog hands the message over instead of awaiting the pod boot", async () => {
  const dialog = await Bun.file(new URL("./components/NewSessionDialog.tsx", import.meta.url)).text();
  expect(dialog).toContain("queueFirstMessage");
  // The await is what made the modal hang; its return is the whole change.
  expect(dialog).not.toContain("await client.postMessage");
});
