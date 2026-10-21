// main() takes its sidecar/runSession dependencies as parameters (defaulting
// to the real implementations) rather than being driven via mock.module() —
// Bun's mock.module is process-global, not per-file, and session.test.ts
// independently needs the real ./session.js and ./sidecarClient.js at its own
// top level; module-mocking either here would leak into that file's import
// and break it.
import { test, expect, afterEach } from "bun:test";

process.env.SESSION_ID = "session-1";
process.env.TARGET_REPO = "dream-analyst";

const { main } = await import("./index.js");

// main() sets the real process.exitCode on a session failure (that's the
// point — see the test below), which tests here trigger on purpose. Without
// resetting, that leaks past this file into bun test's own exit code. Reset
// to 0, not undefined — this Bun version treats assigning `undefined` after a
// prior write as a no-op, so it wouldn't actually clear it.
afterEach(() => {
  process.exitCode = 0;
});

// A recorder for what main() pushes to the transcript. It used to record
// journal writes; main() makes none now (ADR-0055) — knowledge_journal is
// what a future session on a repo needs to know, not lifecycle bookkeeping.
function fakeSidecar(pushed: { text: string; type?: string }[], journal: unknown[] = []) {
  return {
    appendJournal: async (_repo: string, _actor: string, event: string, payload: unknown) => {
      journal.push({ event, payload });
    },
    getSession: async () => ({ description: "test session", permissionMode: "default", model: "claude-opus-4-8" }),
    saveSessionId: async () => {},
    savePermissionMode: async () => {},
    pushMessage: async (_from: string, text: string, type?: string) => {
      pushed.push({ text, type });
      return 1;
    },
    streamHumanMessages: async () => {},
  };
}

// The bug this locks in: a real session failure was once swallowed inside
// main()'s own catch block, so the process still exited 0 — the provisioner's
// GC then saw the Job as Succeeded and discarded it, even though the session
// had failed. The exit code is the one signal that survives when every
// sidecar call has also failed.
test("a real session failure sets a non-zero exit code even though main() handles it internally", async () => {
  const pushed: { text: string; type?: string }[] = [];
  const failing = async () => {
    throw new Error("simulated implementation failure");
  };

  await main(fakeSidecar(pushed) as never, failing as never);

  expect(process.exitCode).toBe(1);
});

// The crash error is the one thing session.* carried that existed nowhere
// else — thrown outside the SDK loop so transcript never saw it, while
// pod_message gets only the generic "worker job reached a terminal Failed
// phase" and Loki is retention-bound. Dropping the journal write must not
// drop the error with it, so it moves to the transcript, which is the surface
// a human actually reads (the dashboard has never rendered the journal).
test("an unexpected failure records its error on the transcript", async () => {
  const pushed: { text: string; type?: string }[] = [];
  const failing = async () => {
    throw new Error("simulated implementation failure");
  };

  await main(fakeSidecar(pushed) as never, failing as never);

  const entry = pushed.find((p) => p.text.includes("session_failed"));
  expect(entry).toBeDefined();
  expect(entry?.type).toBe("system");
  // The actual error, not just that something failed — a generic marker
  // would be no better than the pod_phase the provisioner already reports.
  expect(entry?.text).toContain("simulated implementation failure");
});

// An abort is a human pressing Stop, not a failure. It must not leave a
// scary system entry on the transcript, and must not fail the Job.
test("a human-initiated abort records nothing and exits zero", async () => {
  const pushed: { text: string; type?: string }[] = [];
  const aborted = async () => {
    throw new Error("Request was aborted");
  };

  await main(fakeSidecar(pushed) as never, aborted as never);

  expect(process.exitCode).toBe(0);
  expect(pushed).toEqual([]);
});

// A transcript write failing must not turn a handled failure into an
// unhandled crash — the same class of blip the old status-write guard existed
// for, minus the status write.
test("a sidecar failure while recording the error doesn't crash the process", async () => {
  const brokenSidecar = {
    appendJournal: async () => {
      throw new Error("sidecar unreachable");
    },
    getSession: async () => ({ description: "test session" }),
    saveSessionId: async () => {},
    savePermissionMode: async () => {},
    pushMessage: async () => {
      throw new Error("sidecar unreachable");
    },
    streamHumanMessages: async () => {},
  };
  const failing = async () => {
    throw new Error("simulated implementation failure");
  };

  let exited: number | "not-called" = "not-called";
  process.exit = ((code?: number) => {
    exited = code ?? 0;
    return undefined as never;
  }) as typeof process.exit;

  // Resolves rather than throwing: main() contains the double failure
  // locally and never escapes to the top-level crash handler.
  await main(brokenSidecar as never, failing as never);

  expect(exited).toBe("not-called");
  expect(process.exitCode).toBe(1);
});

// There is no automated completion detection anywhere: a session ends via a
// human's Stop, or by throwing. It reports no status at all now — a
// polymorphic session has no completion the worker is in a position to
// declare, which is why the old enum's 'done' value never acquired a writer.
// And it journals nothing: session.stopped's "summary" was the last assistant
// text block, which session.ts had already pushed to transcript verbatim.
test("a session ending normally writes no status, no journal entry, and exits zero", async () => {
  const pushed: { text: string; type?: string }[] = [];
  const journal: unknown[] = [];
  const stopped = async () => ({ aborted: true, summary: "stopped mid-conversation" });

  await main(fakeSidecar(pushed, journal) as never, stopped as never);

  expect(process.exitCode).toBe(0);
  expect(journal).toEqual([]);
  expect(pushed).toEqual([]);
});
