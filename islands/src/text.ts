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

// A member's label: author_name is their email, shown without the domain.
export function displayName(authorName: string): string {
  const n = authorName.trim();
  if (!n) return "Member";
  const at = n.indexOf("@");
  return at > 0 ? n.slice(0, at) : n;
}
