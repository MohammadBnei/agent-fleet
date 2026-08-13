import type { ButtonHTMLAttributes } from "react";

// Every button here fires a network call, and none of them was telling you
// so: `disabled` alone reads as "nothing happened", especially on a phone
// where there's no hover state to lose either. The three surfaces that own
// decisions (feed card, dock, list) had three different answers — a "…", a
// bare disable, and nothing — so this is one answer instead.
//
// Two flags on purpose:
// - `busy`: THIS button's own action is in flight. Shows the spinner.
// - `disabled`: something else is in flight (the session actions disable as
//   a group — Kill especially shouldn't be clickable while Warm resolves).
//
// The spinner replaces the label *visually* rather than sitting beside it,
// so the button doesn't resize mid-click and shift whatever is under the
// finger. The label stays in the accessible tree either way — dropping it
// while busy would rename "Interrupt" to "working…" for a screen reader
// mid-action, which is exactly when knowing which button you hit matters.
export function ActionButton({
  busy = false,
  disabled = false,
  children,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { busy?: boolean }) {
  return (
    <button type="button" disabled={busy || disabled} aria-busy={busy || undefined} {...rest}>
      {busy ? (
        <span className="inline-flex items-center justify-center">
          <span className="loading loading-spinner loading-xs" />
          <span className="sr-only">{children}</span>
        </span>
      ) : (
        children
      )}
    </button>
  );
}
