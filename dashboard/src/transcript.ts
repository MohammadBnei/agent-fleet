import { TranscriptEntryType, type TranscriptEntry } from "./gen/agentfleet/v1/transcript_pb";

export type QuestionOption = { label: string; description: string };
export type Question = { question: string; header: string; options: QuestionOption[]; multiSelect?: boolean };

// AskUserQuestion (docs/adr/0018) posts `text` as JSON, not prose — these
// parse it defensively so a malformed/future-shaped payload falls back to
// a plain bubble instead of crashing the page.
export function parseQuestions(text: string): Question[] | null {
  try {
    const parsed = JSON.parse(text) as { questions?: unknown };
    return Array.isArray(parsed.questions) ? (parsed.questions as Question[]) : null;
  } catch {
    return null;
  }
}

export function parseAnswers(text: string): Record<string, string> | null {
  try {
    const parsed = JSON.parse(text) as { answers?: unknown };
    return parsed.answers && typeof parsed.answers === "object" ? (parsed.answers as Record<string, string>) : null;
  } catch {
    return null;
  }
}

// Only one question is ever pending per task at a time (the planner's tool
// call blocks on it) — the latest QUESTION entry with no later ANSWER.
export function findPendingQuestion(entries: TranscriptEntry[]): TranscriptEntry | null {
  const idx = entries.findIndex(
    (e, i) =>
      e.type === TranscriptEntryType.QUESTION &&
      !entries.slice(i + 1).some((later) => later.type === TranscriptEntryType.ANSWER),
  );
  return idx >= 0 ? entries[idx] : null;
}

export type ToolCallSummary = { branch?: string; files?: { path: string; added: number; removed: number }[] };

// The sidecar's periodic telemetry push always sends {branch, files[]}
// (sidecar/internal/telemetry/loop.go), but its local HTTP endpoint
// forwards arbitrary caller-supplied JSON as the same entry type too — so
// nothing here is guaranteed present, hence the defensive parse.
export function parseToolCallSummary(text: string): ToolCallSummary | null {
  try {
    const parsed = JSON.parse(text) as ToolCallSummary;
    return typeof parsed === "object" && parsed !== null ? parsed : null;
  } catch {
    return null;
  }
}

// Latest TOOL_CALL entry's summary — the CHANGES panel only ever shows the
// most recent snapshot, not a running diff across every push.
export function latestToolCallSummary(entries: TranscriptEntry[]): ToolCallSummary | null {
  for (let i = entries.length - 1; i >= 0; i--) {
    if (entries[i].type === TranscriptEntryType.TOOL_CALL) {
      return parseToolCallSummary(entries[i].text);
    }
  }
  return null;
}
