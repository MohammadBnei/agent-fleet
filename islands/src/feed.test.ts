import { expect, mock, test } from "bun:test";

// The generated module imports @bufbuild/protobuf, which resolves from
// dashboard/ (not installed in islands CI, see errors.ts): the enum is all
// feed.ts and dashboard/src/transcript.ts read from it at run time.
mock.module("../../dashboard/src/gen/agentfleet/v1/transcript_pb", () => ({
  TranscriptEntryType: {
    UNSPECIFIED: 0,
    DISCUSSION: 1,
    ABORT: 3,
    QUESTION: 4,
    ANSWER: 5,
    INTERRUPT: 14,
    PERMISSION_REQUEST: 12,
    PERMISSION_RESPONSE: 13,
  },
}));

const { decisions, liveState } = await import("./feed");
const { TranscriptEntryType: T } = await import(
  "../../dashboard/src/gen/agentfleet/v1/transcript_pb"
);

type Entry = Parameters<typeof decisions>[0][number];
const entry = (seq: bigint, type: number, text = "") =>
  ({ seq, type, text, from: "agent" }) as unknown as Entry;

// A stamped request outlives an interrupt (docs/adr/0082 §5), so only the
// decisions store can say it was closed: withdrawn, it leaves no row.
test("a stamped interrupted request is open until withdrawn", () => {
  const entries = [
    entry(
      5n,
      T.PERMISSION_REQUEST,
      JSON.stringify({ tool: "Bash", input: { command: "ls" }, nb: 1 }),
    ),
    entry(6n, T.INTERRUPT),
  ];
  expect(decisions(entries).map((d) => d.seq)).toEqual([5n]);
  expect(decisions(entries, new Set([5n]))).toEqual([]);
});

test("an open decision never reads waiting over work", () => {
  const ask = entry(5n, T.PERMISSION_REQUEST, "{}");
  const said = entry(6n, T.DISCUSSION, "carrying on");
  expect(liveState([ask, said], 1, "working")).toBe("working");
  expect(liveState([ask], 1, "working")).toBe("waiting");
  expect(liveState([ask, said], 1, "idle")).toBe("waiting");
  expect(liveState([ask, said], 0, "idle")).toBe("idle");
});
