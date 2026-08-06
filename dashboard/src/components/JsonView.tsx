// Formatted stand-in for JSON.stringify(x, null, 2) in tool call CALL/OUTPUT
// panels — arbitrary unknown shapes (whichever Claude Agent SDK tool ran),
// so this stays fully recursive/generic rather than assuming a schema.
// ponytail: opacity-only tinting, not full syntax highlighting — revisit if
// output review becomes a real workflow (a rainbow scheme would also fight
// ToolCallItem's error-red wrapper coloring).
export function JsonView({ value, depth = 0 }: { value: unknown; depth?: number }) {
  if (value === null || value === undefined) {
    return <span className="text-base-content/40">{String(value)}</span>;
  }
  if (typeof value === "boolean" || typeof value === "number") {
    return <span className="text-base-content/85">{String(value)}</span>;
  }
  if (typeof value === "string") {
    if (value.includes("\n")) {
      return <div className="whitespace-pre-wrap text-base-content/85">{value}</div>;
    }
    return <span className="text-base-content/85">"{value}"</span>;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return <span className="text-base-content/40">[ ]</span>;
    return (
      <div className="flex flex-col">
        {value.map((item, i) => (
          <div key={i} className="pl-3">
            <span className="text-base-content/50">- </span>
            <JsonView value={item} depth={depth + 1} />
          </div>
        ))}
      </div>
    );
  }
  if (typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return <span className="text-base-content/40">{"{ }"}</span>;
    return (
      <div className="flex flex-col">
        {entries.map(([key, v]) => (
          <div key={key} className="pl-3">
            <span className="text-base-content/50">{key}: </span>
            <JsonView value={v} depth={depth + 1} />
          </div>
        ))}
      </div>
    );
  }
  return <span className="text-base-content/85">{String(value)}</span>;
}
