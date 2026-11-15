// A small, non-blocking note that a pod is booting. Sending a message or
// pressing Warm on a cold session takes seconds server-side; nothing else is
// locked meanwhile, so this is only here to say why the reply is not instant.
//
// The <output> stays mounted and only its content toggles: a live region that
// appears together with its text is often not announced.
export function PodStarting({
  active,
  className = "",
}: {
  active: boolean;
  className?: string;
}) {
  return (
    <output
      className={`flex items-center gap-2 text-xs text-dim2 ${active ? className : ""}`}
    >
      {active && (
        <>
          <span
            aria-hidden
            className="size-3 flex-none rounded-full border-2 border-current border-r-transparent animate-spin"
          />
          starting pod…
        </>
      )}
    </output>
  );
}
