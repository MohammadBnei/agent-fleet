import { JsonView } from "./JsonView";

// What a tool call is actually about to do, rendered per tool instead of as
// one JSON.stringify blob. This is the text a human reads before clicking
// Allow, so an Edit has to show what changes — a permission prompt whose
// content is unreadable trains people to approve without looking.

type Line = { kind: "context" | "add" | "remove"; text: string };

// Line-level LCS. No diff library: the inputs here are one tool call's
// before/after strings, and the whole thing is shorter than the adapter
// wiring a dependency would need.
export function diffLines(before: string, after: string): Line[] {
  // "".split("\n") is [""], not [] — without this a Write (which has no
  // before at all) opens with a phantom removed blank line.
  if (before === "") return after.split("\n").map((text) => ({ kind: "add", text }));
  const a = before.split("\n");
  const b = after.split("\n");
  // lcs[i][j] = length of the longest common subsequence of a[i:] and b[j:]
  const lcs: number[][] = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }
  const out: Line[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      out.push({ kind: "context", text: a[i] });
      i++;
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) {
      out.push({ kind: "remove", text: a[i++] });
    } else {
      out.push({ kind: "add", text: b[j++] });
    }
  }
  while (i < a.length) out.push({ kind: "remove", text: a[i++] });
  while (j < b.length) out.push({ kind: "add", text: b[j++] });
  return out;
}

export function DiffView({ before, after }: { before: string; after: string }) {
  const lines = diffLines(before, after);
  return (
    <div className="overflow-x-auto rounded-md bg-base-200/40">
      <pre className="text-[10.5px] leading-[1.5] font-mono min-w-full w-max">
        {lines.map((line, idx) => (
          <div
            key={idx}
            className={
              line.kind === "add"
                ? "bg-success/15 text-success"
                : line.kind === "remove"
                  ? "bg-error/15 text-error"
                  : "text-base-content/50"
            }
          >
            <span className="select-none opacity-50">{line.kind === "add" ? "+" : line.kind === "remove" ? "-" : " "} </span>
            {line.text || " "}
          </div>
        ))}
      </pre>
    </div>
  );
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex gap-2 text-[10.5px]">
      <span className="text-base-content/40 flex-none">{label}</span>
      <span className="text-base-content/80 break-all">{value}</span>
    </div>
  );
}

export function ToolInputView({ tool, input }: { tool: string; input: unknown }) {
  const obj = (input && typeof input === "object" ? input : {}) as Record<string, unknown>;
  const str = (key: string): string | undefined => (typeof obj[key] === "string" ? (obj[key] as string) : undefined);

  const filePath = str("file_path");

  if (tool === "Edit" && typeof obj.old_string === "string" && typeof obj.new_string === "string") {
    return (
      <div className="flex flex-col gap-1.5">
        {filePath && <Field label="file" value={filePath} />}
        {obj.replace_all === true && <Field label="mode" value="replace all occurrences" />}
        <DiffView before={obj.old_string} after={obj.new_string} />
      </div>
    );
  }

  if (tool === "Write" && typeof obj.content === "string") {
    return (
      <div className="flex flex-col gap-1.5">
        {filePath && <Field label="file" value={filePath} />}
        {/* A Write has no "before" of its own — every line is new. */}
        <DiffView before="" after={obj.content} />
      </div>
    );
  }

  if (tool === "Bash" && typeof obj.command === "string") {
    return (
      <div className="flex flex-col gap-1.5">
        {str("description") && <div className="text-[10.5px] text-base-content/50">{str("description")}</div>}
        <div className="overflow-x-auto rounded-md bg-base-200/40 p-2">
          <pre className="text-[11px] font-mono text-base-content/90 w-max min-w-full">{obj.command}</pre>
        </div>
        {obj.run_in_background === true && <Field label="mode" value="background" />}
      </div>
    );
  }

  const simple = [
    ["file", filePath],
    ["path", str("path")],
    ["pattern", str("pattern")],
    ["url", str("url")],
    ["query", str("query")],
  ].filter((entry): entry is [string, string] => Boolean(entry[1]));

  // Only claim to have rendered the input if nothing else is in it —
  // a partial summary that hides an unshown field is worse than raw JSON
  // on a screen whose whole job is "decide whether to allow this".
  if (simple.length > 0 && Object.keys(obj).every((k) => ["file_path", "path", "pattern", "url", "query", "description", "limit", "offset"].includes(k))) {
    return (
      <div className="flex flex-col gap-1">
        {simple.map(([label, value]) => (
          <Field key={label} label={label} value={value} />
        ))}
        {str("description") && <div className="text-[10.5px] text-base-content/50">{str("description")}</div>}
      </div>
    );
  }

  return <JsonView value={input} />;
}
