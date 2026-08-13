import type { ReactNode } from "react";

type CollapseProps = {
  summary: ReactNode;
  children: ReactNode;
  className?: string;
  summaryClassName?: string;
  contentClassName?: string;
};

// The one collapsible primitive for the whole dashboard — native
// <details>/<summary>, no arrow icon. A collapsed row's own background tint
// (behind the text, not competing with it) is the only "this expands"
// signal; opening it just switches which background shows.
export function Collapse({ summary, children, className = "", summaryClassName = "", contentClassName = "" }: CollapseProps) {
  return (
    <details className={`collapse border border-base-content/10 bg-base-200/40 rounded-md text-xs ${className}`}>
      {/* No padding baked in here — callers own it via summaryClassName,
          since stacking a default padding utility with an overriding one
          risks Tailwind's generated-CSS order deciding the winner instead
          of the more specific one (bit us once already, see the mobile
          scroll button fix). */}
      <summary className={`collapse-title min-h-0 cursor-pointer bg-base-content/5 hover:bg-base-content/10 ${summaryClassName}`}>
        {summary}
      </summary>
      <div className={`collapse-content px-2 pb-1 ${contentClassName}`}>{children}</div>
    </details>
  );
}
