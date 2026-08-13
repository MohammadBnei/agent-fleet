// The mockups' one selection control: a row of cells inside a single 1px
// border, dividers between them, the active cell filled with the accent. Used
// for the theme switch, the desktop view nav, the DENSITY toggle on both form
// factors, and the mobile bottom tab bar (same idiom, `grow` cells).

export function Segmented<T extends string>({
  value,
  options,
  onChange,
  grow = false,
  size = "md",
  className = "",
}: {
  value: T;
  options: readonly { value: T; label: string; title?: string }[];
  onChange: (value: T) => void;
  // Bottom tab bar / mobile density: cells split the available width.
  grow?: boolean;
  size?: "sm" | "md" | "lg";
  className?: string;
}) {
  const pad =
    size === "sm" ? "px-2 py-0.5 text-2xs" : size === "lg" ? "px-0 py-2.5 text-xs" : "px-3 py-1 text-xs";
  return (
    <div className={`flex border border-line ${className}`}>
      {options.map((opt, i) => {
        const active = opt.value === value;
        return (
          <button
            key={opt.value}
            type="button"
            title={opt.title}
            aria-pressed={active}
            onClick={() => onChange(opt.value)}
            className={`${pad} cursor-pointer whitespace-nowrap ${grow ? "grow text-center" : ""} ${
              i > 0 ? "border-l border-line" : ""
            } ${active ? "bg-primary text-primary-content font-medium" : "text-dim hover:text-base-content"}`}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}
