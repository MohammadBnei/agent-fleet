// A mock AgentService for the demo page and for eyeballing the island before
// the real methods are live. Not part of the bundle (vite build uses src/main.ts).
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { createRouterTransport } from "@connectrpc/connect";
import { setTransport } from "../src/main";
import { AgentService, EntrySchema, ThreadSchema, StatusSchema, type Entry } from "../../dashboard/src/gen/agentfleet/v1/agent_pb";
import { TranscriptEntryType as T } from "../../dashboard/src/gen/agentfleet/v1/transcript_pb";

let seq = 0n;
const entry = (type: T, from: string, text: string, extra: MessageInitShape<typeof EntrySchema> = {}) =>
  create(EntrySchema, { seq: ++seq, type, from, text, createdAt: new Date().toISOString(), ...extra });

const log: Entry[] = [
  entry(T.DISCUSSION, "human", "Can you tidy the onboarding page and link it from the index?", { authorName: "Nadia" }),
  entry(
    T.DISCUSSION,
    "agent",
    "Sure. The page has **three** stale sections. Here is how the pages link today:\n\n```mermaid\ngraph LR; Index-->Onboarding; Onboarding-->Tools; Onboarding-->Contacts\n```\n\nI will merge *Tools* into the onboarding page. `Contacts` stays separate.",
  ),
  // Hostile: a directive trying to turn the Allow button into a page overlay.
  entry(
    T.DISCUSSION,
    "agent",
    '```mermaid\n%%{init: {"themeCSS": ".decision button{position:fixed;inset:0;opacity:0.4;background:red}"}}%%\ngraph TD; Ask-->Answer\n```',
  ),
  entry(
    T.QUESTION,
    "agent",
    JSON.stringify({
      questions: [
        {
          question: "Keep the old Tools page as a redirect?",
          header: "Tools page",
          options: [
            { label: "Keep a redirect", description: "Old links keep working." },
            { label: "Delete it", description: "Cleaner space, old links break." },
          ],
        },
      ],
    }),
  ),
  entry(T.PERMISSION_REQUEST, "agent", JSON.stringify({ tool: "Bash", input: { command: "git push origin main" } })),
];

const thread = (id: string, title: string, visibility: string) =>
  create(ThreadSchema, { id, definition: "opus-workspace", title, visibility, mine: true, status: create(StatusSchema, { state: "idle" }) });
const threads = [thread("a1b2c3", "Onboarding cleanup", "shared"), thread("d4e5f6", "", "private")];

const listeners = new Set<(e: Entry) => void>();
const push = (e: Entry) => {
  log.push(e);
  for (const l of listeners) l(e);
};
const say = (msg: string) => ((document.getElementById("log") as HTMLElement).textContent = msg);

setTransport(
  createRouterTransport(({ service }) => {
    service(AgentService, {
      listThreads: () => ({ threads }),
      createThread: (req) => {
        const t = thread(Math.random().toString(16).slice(2, 8), req.title, req.visibility);
        threads.unshift(t);
        return { thread: t };
      },
      getTranscript: () => ({ entries: [...log], nextSeq: seq + 1n }),
      async *streamTranscript(req) {
        // Inclusive, like core's ReadSince.
        const queue: Entry[] = log.filter((e) => e.seq >= req.sinceSeq);
        let wake: (() => void) | undefined;
        const l = (e: Entry) => {
          queue.push(e);
          wake?.();
        };
        listeners.add(l);
        try {
          for (;;) {
            while (queue.length) yield { entry: queue.shift() };
            await new Promise<void>((r) => (wake = r));
          }
        } finally {
          listeners.delete(l);
        }
      },
      postMessage: (req) => {
        push(entry(T.DISCUSSION, "human", req.text, { authorName: "You" }));
        setTimeout(() => push(entry(T.DISCUSSION, "agent", `Noted: _${req.text}_. Working on it.`)), 900);
        return { seq };
      },
      answerQuestion: (req) => {
        say(`AnswerQuestion seq=${req.seq} ${req.answersJson}`);
        push(entry(T.ANSWER, "human", req.answersJson, { replyTo: req.seq }));
        return { seq };
      },
      respondToPermission: (req) => {
        say(`RespondToPermission seq=${req.seq} ${req.behavior}`);
        push(entry(T.PERMISSION_RESPONSE, "human", JSON.stringify({ behavior: req.behavior }), { replyTo: req.seq }));
        return { seq };
      },
      approvePlan: (req) => {
        say(`ApprovePlan seq=${req.seq}`);
        push(entry(T.PERMISSION_RESPONSE, "human", JSON.stringify({ behavior: "allow" }), { replyTo: req.seq }));
        return { seq };
      },
    });
  }),
);

const el = document.createElement("fleet-agent");
el.setAttribute("definition", "opus-workspace");
el.setAttribute("context", "Onboarding");
el.addEventListener("fleet-status", (e) => say(`fleet-status ${JSON.stringify((e as CustomEvent).detail)}`));
document.getElementById("slot")!.append(el);
document.getElementById("theme")!.addEventListener("change", (e) => el.setAttribute("theme", (e.target as HTMLSelectElement).value));
