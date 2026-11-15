// A small, non-blocking note that a pod is booting. Sending a message or
// pressing Warm on a cold session takes seconds server-side; nothing else is
// locked meanwhile, so this is only here to say why the reply is not instant.
export function PodStarting({ className = "" }: { className?: string }) {
  return (
    <output
      className={`flex items-center gap-2 text-xs text-dim2 ${className}`}
    >
      <span className="loading loading-spinner loading-xs" />
      starting pod…
    </output>
  );
}
