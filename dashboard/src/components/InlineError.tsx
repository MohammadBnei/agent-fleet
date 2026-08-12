// A page-level failure that must NOT trap the reader.
//
// Worktrees/Files/Audits used the shared ErrorModal for load failures, which
// covers the screen — and now that the mobile tab bar is the only way out of a
// view, an unreachable provisioner or object store meant the modal blocked every
// route off the page. A load error is information, not a decision: it belongs on
// the page, next to the thing that failed, with a retry.
export function InlineError({
  message,
  onRetry,
  onDismiss,
}: {
  message: string | null;
  onRetry?: () => void;
  onDismiss?: () => void;
}) {
  if (!message) return null;
  return (
    <div className="border border-orange-line bg-orange-bg px-3.5 py-3 flex gap-2.5 items-start">
      <span className="text-warning text-[12px] flex-none">!</span>
      <div className="text-[12px] text-warning leading-[1.55] min-w-0 flex-1 break-words">{message}</div>
      {onRetry && (
        <button
          type="button"
          onClick={onRetry}
          className="flex-none border border-orange-line px-2.5 py-1 text-[11.5px] text-warning"
        >
          retry
        </button>
      )}
      {onDismiss && (
        <button
          type="button"
          onClick={onDismiss}
          aria-label="Dismiss error"
          className="flex-none text-dim2 hover:text-warning text-[11px]"
        >
          ✕
        </button>
      )}
    </div>
  );
}
