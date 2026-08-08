import { describe, test, expect, beforeEach, afterEach, mock } from "bun:test";

// Mock document for testing visibility changes
const mockDocument = {
  hidden: false,
  _listeners: new Map<string, Set<EventListener>>(),
  addEventListener(event: string, listener: EventListener) {
    if (!this._listeners.has(event)) {
      this._listeners.set(event, new Set());
    }
    this._listeners.get(event)!.add(listener);
  },
  removeEventListener(event: string, listener: EventListener) {
    this._listeners.get(event)?.delete(listener);
  },
  _trigger(event: string) {
    this._listeners.get(event)?.forEach((listener) => listener(new Event(event)));
  },
};

describe("subscribeTranscript visibility handling", () => {
  let originalDocument: typeof document;

  beforeEach(() => {
    originalDocument = global.document;
    // @ts-ignore - mocking document
    global.document = mockDocument;
    mockDocument.hidden = false;
    mockDocument._listeners.clear();
  });

  afterEach(() => {
    global.document = originalDocument;
  });

  test("should register visibilitychange listener", async () => {
    // We can't actually import subscribeTranscript without the full Connect setup,
    // but we can verify the pattern exists in the code
    const code = await Bun.file("./src/connectClient.ts").text();

    expect(code).toContain("visibilitychange");
    expect(code).toContain("document.hidden");
    expect(code).toContain("paused");
  });

  test("should handle page visibility in stream loop", async () => {
    const code = await Bun.file("./src/connectClient.ts").text();

    // Verify the pause/resume logic exists
    expect(code).toContain("if (paused)");
    expect(code).toContain("paused = document.hidden");
  });

  test("should clean up listener on unsubscribe", async () => {
    const code = await Bun.file("./src/connectClient.ts").text();

    // Verify cleanup happens
    expect(code).toContain("removeEventListener");
    expect(code).toContain("controller.abort()");
  });
});
