import { useState } from "react";

const TRACE = "\n\nTrace ID: ";

// An error box's text. errorText ends it with the trace id; that line gets a copy button, and the id selects
// whole on one click, so a reported failure can be pasted into the logs search.
export function ErrorMessage({ message }: { message: string }) {
  const [copied, setCopied] = useState(false);
  const at = message.lastIndexOf(TRACE);
  if (at < 0) return <>{message}</>;
  const id = message.slice(at + TRACE.length);
  const copy = () =>
    navigator.clipboard?.writeText(id).then(
      () => setCopied(true),
      () => {},
    );
  return (
    <>
      {message.slice(0, at)}
      <span className="mt-3 flex flex-wrap items-center gap-x-2">
        Trace ID:<span className="select-all break-all">{id}</span>
        <button
          type="button"
          onClick={copy}
          aria-label="Copy trace ID"
          className="border border-current px-1.5 text-xs opacity-80 hover:opacity-100"
        >
          {copied ? "copied" : "copy"}
        </button>
      </span>
    </>
  );
}
