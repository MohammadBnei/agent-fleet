// Busy keys are `permission:${seq}`, `question:${seq}`, `action:warm`... and
// repeat across sessions. One hook instance outlives a switch from session A to
// B while A's calls are still in flight, so its keys are stored scoped to the
// session and only the current session's are ever read back.
export const scopeKey = (sessionId: string, key: string) =>
  `${sessionId}|${key}`;

export function keysOf(sessionId: string, all: readonly string[]): string[] {
  const prefix = `${sessionId}|`;
  return all
    .filter((k) => k.startsWith(prefix))
    .map((k) => k.slice(prefix.length));
}

// A message that did not go comes back in front of whatever was typed since.
export const restoreDraft = (text: string, current: string) =>
  current ? `${text}\n${current}` : text;
