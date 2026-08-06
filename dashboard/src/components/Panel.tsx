import { useState } from "react";

export type PanelSize = { height: number };

type PanelProps = {
  id: string;
  title: string;
  headerExtra?: React.ReactNode;
  size: PanelSize;
  onResize: (id: string, size: PanelSize) => void;
  collapsed: boolean;
  onToggleCollapse: () => void;
  bodyRef?: React.Ref<HTMLDivElement>;
  onBodyScroll?: (e: React.UIEvent<HTMLDivElement>) => void;
  overlay?: React.ReactNode;
  children: React.ReactNode;
  // Root wrapper ref — lets the sidebar's "fit height" button measure each
  // panel's real rendered chrome (header + padding, everything but the
  // resizable body) instead of guessing at pixel constants.
  rootRef?: React.Ref<HTMLDivElement>;
};

// Fixed order (no drag-to-reorder) — a collapsed panel shrinks to just its
// header, freeing up column space for the others. Height resizes via a
// dedicated drag handle below the body — same mechanism and look as
// TaskDetail.tsx's sidebar/exchange-zone split resizer (live-drag local
// state, committed on mouseup), not the browser's native CSS resize corner.
// Width isn't resizable per-panel — the whole sidebar's width against the
// exchange zone is (that same split resizer).
export function Panel({ id, title, headerExtra, size, onResize, collapsed, onToggleCollapse, bodyRef, onBodyScroll, overlay, children, rootRef }: PanelProps) {
  const [liveHeight, setLiveHeight] = useState<number | null>(null);
  const displayHeight = liveHeight ?? size.height;

  function startResize(e: React.MouseEvent) {
    e.preventDefault();
    const startY = e.clientY;
    const startHeight = size.height;
    function onMove(ev: MouseEvent) {
      setLiveHeight(Math.max(72, startHeight + (ev.clientY - startY)));
    }
    function onUp() {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
      setLiveHeight((v) => {
        if (v !== null) onResize(id, { height: v });
        return null;
      });
    }
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  return (
    <div ref={rootRef} className="rounded-lg border border-base-content/10 bg-base-300/20 p-1.5">
      <div className="flex items-center gap-1.5">
        <button
          type="button"
          onClick={onToggleCollapse}
          title={collapsed ? "Expand" : "Collapse"}
          className="flex-none w-4 text-center text-base-content/40 hover:text-base-content/70 cursor-pointer select-none"
        >
          {collapsed ? "▸" : "▾"}
        </button>
        <div className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold flex-1">{title}</div>
        {headerExtra}
      </div>
      {!collapsed && (
        <div className="relative mt-1">
          <div
            ref={bodyRef}
            onScroll={onBodyScroll}
            style={{ height: displayHeight, minHeight: 72 }}
            className="overflow-y-auto flex flex-col gap-1"
          >
            {children}
          </div>
          {overlay}
          <div
            onMouseDown={startResize}
            title="Drag to resize"
            className="h-1.5 mt-0.5 rounded cursor-row-resize bg-base-content/5 hover:bg-primary/40 active:bg-primary/50"
          />
        </div>
      )}
    </div>
  );
}
