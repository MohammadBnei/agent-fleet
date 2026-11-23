import { expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import {
  type TranscriptEntry,
  TranscriptEntryType,
} from "../gen/agentfleet/v1/transcript_pb";
import { feedVisibility } from "../transcript";
import { SessionFeed } from "./SessionFeed";
import { TranscriptEntryView } from "./TranscriptEntryView";

let seq = 100n;
function entry(
  type: TranscriptEntryType,
  text: string,
  from = "agent",
  replyTo?: bigint,
): TranscriptEntry {
  seq += 1n;
  return {
    $typeName: "agentfleet.v1.TranscriptEntry",
    sessionId: "t",
    seq,
    from,
    text,
    type,
    replyTo,
  } as TranscriptEntry;
}

function feed(entries: TranscriptEntry[], over: { dock?: boolean } = {}) {
  return renderToStaticMarkup(
    <SessionFeed
      entries={entries}
      visibility={feedVisibility("decisions", false)}
      density="decisions"
      isBusy={() => false}
      dockPendingDecision={over.dock}
      onRespond={() => {}}
      onAnswer={() => {}}
      onApprovePlan={() => {}}
      onPlanFeedback={() => {}}
    />,
  );
}

const perm = () =>
  entry(
    TranscriptEntryType.PERMISSION_REQUEST,
    JSON.stringify({ tool: "Bash", input: { command: "curl x" }, nb: 1 }),
  );

test("a decision waiting in the dock is something at the decisions density", () => {
  const html = feed([perm()], { dock: true });
  expect(html).not.toContain("Nothing at this density");
  expect(html).toContain("A decision is waiting below");
  // No decision, nothing shown: the old line stays.
  expect(feed([])).toContain("Nothing at this density");
});

test("a parked call reads as waiting, then allowed, never failed", () => {
  const call = entry(
    TranscriptEntryType.ASSISTANT,
    JSON.stringify({ id: "tu1", tool: "Bash", input: { command: "curl x" } }),
  );
  const parkedResult = entry(
    TranscriptEntryType.USER,
    JSON.stringify({
      toolUseId: "tu1",
      isError: true,
      content: "Pending (request seq 7). Do not retry; a message arrives.",
    }),
    "agent",
  );
  const req = { ...perm(), seq: 7n } as TranscriptEntry;
  const visible = (entries: TranscriptEntry[]) =>
    renderToStaticMarkup(
      <SessionFeed
        entries={entries}
        visibility={feedVisibility("everything", false)}
        density="everything"
        isBusy={() => false}
        onRespond={() => {}}
        onAnswer={() => {}}
        onApprovePlan={() => {}}
        onPlanFeedback={() => {}}
      />,
    );
  const waiting = visible([call, parkedResult, req]);
  expect(waiting).toContain("waiting for you");
  expect(waiting).not.toContain(">failed<");
  const allow = entry(
    TranscriptEntryType.PERMISSION_RESPONSE,
    '{"behavior":"allow"}',
    "human",
    7n,
  );
  const allowed = visible([call, parkedResult, req, allow]);
  expect(allowed).toContain(">allowed<");
  expect(allowed).not.toContain(">failed<");
});

test("the per-model dollar line is hidden on the gateway route", () => {
  const result = (route: string) =>
    entry(
      TranscriptEntryType.RESULT,
      JSON.stringify({
        subtype: "success",
        numTurns: 1,
        route,
        totalCostUsd: route === "gateway" ? null : 1,
        modelUsage: {
          "deepseek-v4-1-flash": { costUSD: 2.671 },
          "z-ai-glm-5-3": { costUSD: 0.721 },
        },
      }),
    );
  const view = (route: string) => {
    const e = result(route);
    return renderToStaticMarkup(<TranscriptEntryView entry={e} />);
  };
  expect(view("gateway")).not.toContain("$2.671");
  expect(view("subscription")).toContain("$2.671");
});
