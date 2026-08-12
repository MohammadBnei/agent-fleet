// The mockups' recurring container: a bordered box whose label straddles the
// top border, sitting in a gap punched out of it. The label's background has
// to match whatever the card sits on (--panel, i.e. base-200) or the border
// shows through behind the text.
//
// Used by the NEEDS YOU list cards, the desktop permission card, the mobile
// decision dock and the E2E PREVIEW panel.

const TONES = {
  pink: { border: "border-pink-line", bg: "bg-pink-bg", label: "text-error" },
  orange: { border: "border-orange-line", bg: "bg-orange-bg", label: "text-warning" },
  dim: { border: "border-line", bg: "bg-transparent", label: "text-dim2" },
} as const;

export type NotchTone = keyof typeof TONES;

export function NotchCard({
  label,
  tone = "dim",
  className = "",
  labelBg = "bg-base-200",
  children,
}: {
  label: string;
  tone?: NotchTone;
  className?: string;
  // Override when the card sits on something other than base-200 — the notch
  // has to paint over the border it interrupts.
  labelBg?: string;
  children: React.ReactNode;
}) {
  const t = TONES[tone];
  return (
    <div className={`relative border ${t.border} ${t.bg} ${className}`}>
      <div
        className={`absolute -top-[7px] left-3 sm:left-3.5 px-[7px] text-[10px] sm:text-[10.5px] tracking-[0.1em] whitespace-nowrap ${labelBg} ${t.label}`}
      >
        {label}
      </div>
      {children}
    </div>
  );
}
