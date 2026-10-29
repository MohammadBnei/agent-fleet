// Plain text helpers, apart from feed.ts so tests need no generated code.
// A thread title from a message's first line, at most `max` UTF-8 bytes
// (CreateThread refuses over 200), cut on a code point, never mid-character.
export function threadTitle(text: string, max = 120): string {
  const line = text.split("\n")[0].trim();
  const enc = new TextEncoder();
  let out = "";
  for (const ch of line) {
    if (enc.encode(out + ch).length > max) break;
    out += ch;
  }
  return out;
}

// Members' labels: author_name is their email, shown without the domain
// unless two members in the same thread share that part, then in full. No
// name at all reads "Another member", never a name someone else could have.
export function displayNames(authorNames: string[]): Map<string, string> {
  const local = (n: string) => {
    const at = n.indexOf("@");
    return at > 0 ? n.slice(0, at) : n;
  };
  const owners = new Map<string, Set<string>>();
  for (const raw of authorNames) {
    const n = raw.trim();
    if (!n) continue;
    const l = local(n);
    owners.set(l, (owners.get(l) ?? new Set()).add(n));
  }
  const out = new Map<string, string>();
  for (const raw of authorNames) {
    const n = raw.trim();
    out.set(
      raw,
      !n
        ? "Another member"
        : (owners.get(local(n))?.size ?? 0) > 1
          ? n
          : local(n),
    );
  }
  return out;
}

// Typing while a question is open answers it, as "Other" does in Claude Code:
// an option already picked keeps its question, the text answers the rest.
export function withOther(
  questions: string[],
  picked: Record<string, string>,
  text: string,
): Record<string, string> {
  return Object.fromEntries(questions.map((q) => [q, picked[q] || text]));
}

// An answer entry's text for the feed: the distinct answers, one per line.
export function answerText(json: string): string {
  try {
    const a = (JSON.parse(json) as { answers?: unknown }).answers;
    if (a && typeof a === "object") {
      const lines = Object.values(a).map((v) =>
        typeof v === "string" ? v : JSON.stringify(v),
      );
      return [...new Set(lines)].join("\n");
    }
  } catch {
    // Not ours to fix: show it as written.
  }
  return json;
}
