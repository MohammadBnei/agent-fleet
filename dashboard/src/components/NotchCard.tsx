// The mockups' recurring container: a bordered box whose label straddles the
// top border, sitting in a gap punched out of it. The label's background has
// to match whatever the card sits on (--panel, i.e. base-200) or the border
// shows through behind the text.
//
// Used by the NEEDS YOU cards on both form factors — the only users left, so
// it has one tone. The orange and dim tones served cards that are gone (the
// desktop permission card, the mobile dock's notch, the E2E PREVIEW panel).
export function NotchCard({
  label,
  className = "",
  labelBg = "bg-base-200",
  children,
}: {
  label: string;
  className?: string;
  // Override when the card sits on something other than base-200 — the notch
  // has to paint over the border it interrupts.
  labelBg?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={`relative border border-pink-line bg-pink-bg ${className}`}>
      <div
        className={`absolute -top-[7px] left-3 sm:left-3.5 px-[7px] text-2xs tracking-[0.1em] whitespace-nowrap text-error ${labelBg}`}
      >
        {label}
      </div>
      {children}
    </div>
  );
}
