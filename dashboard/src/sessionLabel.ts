// What to print where a session's name goes.
//
// Both fields are optional now (docs/adr/0048 §1: CreateSession takes an
// optional title and nothing else), and `description` only survives on rows
// created before the rewrite. Neither is ever part of the prompt — the
// session's actual instruction is its first transcript entry — so this is
// purely a list label.
//
// Lives here rather than inline because desktop and mobile each render the
// same information from their own components: a fallback inlined in one is a
// blank row in the other.
export function sessionLabel(s: { title?: string; description?: string } | undefined | null): string {
  return s?.title?.trim() || s?.description?.trim() || "untitled session";
}
