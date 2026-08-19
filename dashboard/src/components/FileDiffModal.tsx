import { Modal } from "./Modal";
import { ToolInputView } from "./ToolInputView";
import { fileEdits } from "../transcript";
import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";

// What the CHANGES panel's `+12 −3` actually was. Same renderer the feed uses
// when you expand an Edit — ToolInputView already turns Edit's old_string/
// new_string and Write's content into a DiffView, so this is a container, not
// a second diff implementation.
//
// Every edit to the file, in order, rather than only the last: a file the agent
// touched five times was five decisions, and the net result hides four of them.
export function FileDiffModal({
  path,
  entries,
  onClose,
}: {
  // null = closed. Carrying the path rather than a boolean means the modal has
  // nothing to remember when it reopens on a different file.
  path: string | null;
  entries: TranscriptEntry[];
  onClose: () => void;
}) {
  const edits = path ? fileEdits(entries, path) : [];
  return (
    <Modal open={path !== null} onClose={onClose} boxClassName="max-w-4xl">
      <div className="flex flex-col gap-3">
        <div className="flex items-baseline gap-2 min-w-0">
          <span className="text-2xs tracking-[0.12em] text-dim2 flex-none">DIFF</span>
          <span className="text-sm text-base-content min-w-0 break-all">{path}</span>
          {edits.length > 1 && (
            <span className="text-xs text-dim2 flex-none ml-auto">{edits.length} edits</span>
          )}
        </div>
        <div className="max-h-[70vh] overflow-auto flex flex-col gap-4">
          {edits.length === 0 ? (
            // Not an empty box: the absence is informative, and it is the
            // expected outcome for a file a Bash command rewrote. See the
            // ponytail note on fileEdits.
            <div className="text-sm text-dim2 leading-[1.6]">
              No captured diff for this file. The line counts come from git, but the change
              did not go through an Edit or Write tool call — a Bash command (sed, mv, a
              codegen script, a formatter) leaves nothing for the console to show.
            </div>
          ) : (
            edits.map((e, i) => (
              <div key={String(e.seq)} className="flex flex-col gap-1.5 min-w-0">
                {edits.length > 1 && (
                  <span className="text-2xs tracking-[0.1em] text-dim2">
                    {e.tool.toUpperCase()} {i + 1} OF {edits.length}
                  </span>
                )}
                <ToolInputView tool={e.tool} input={e.input} />
              </div>
            ))
          )}
        </div>
      </div>
    </Modal>
  );
}
