import { useRef } from "react";

type DragHandlers = {
  isDragging: boolean;
  isDragOver: boolean;
  onDragStart: (id: string) => void;
  onDragEnd: () => void;
  onDragEnter: (id: string) => void;
  onDragLeave: () => void;
  onDrop: (id: string) => void;
};

type PanelProps = {
  id: string;
  title: string;
  headerExtra?: React.ReactNode;
  height: number;
  onResize: (id: string, height: number) => void;
  drag: DragHandlers;
  bodyRef?: React.Ref<HTMLDivElement>;
  onBodyScroll?: (e: React.UIEvent<HTMLDivElement>) => void;
  overlay?: React.ReactNode;
  children: React.ReactNode;
};

// Drag is initiated only from the ⠿ handle (never the whole card) so it
// never hijacks interactive content inside a panel — ToolCallItem's
// <details>, TODOS's scroll-to-bottom button, etc. Resizing is native CSS
// (resize-y + overflow-y-auto) — the browser owns the drag gesture, we just
// capture the resulting height on mouseup.
export function Panel({ id, title, headerExtra, height, onResize, drag, bodyRef, onBodyScroll, overlay, children }: PanelProps) {
  const cardRef = useRef<HTMLDivElement>(null);
  return (
    <div
      ref={cardRef}
      onDragOver={(e) => e.preventDefault()}
      onDragEnter={() => drag.onDragEnter(id)}
      onDragLeave={drag.onDragLeave}
      onDrop={(e) => {
        e.preventDefault();
        drag.onDrop(id);
      }}
      className={`rounded-lg border p-3 bg-base-300/20 transition-colors ${
        drag.isDragOver ? "border-primary/40" : "border-base-content/10"
      } ${drag.isDragging ? "opacity-40" : ""}`}
    >
      <div className="flex items-center gap-1.5">
        <span
          draggable
          onDragStart={(e) => {
            e.dataTransfer.setData("text/plain", id);
            e.dataTransfer.effectAllowed = "move";
            if (cardRef.current) e.dataTransfer.setDragImage(cardRef.current, 12, 12);
            drag.onDragStart(id);
          }}
          onDragEnd={drag.onDragEnd}
          title="Drag to reorder"
          className="cursor-grab active:cursor-grabbing text-base-content/30 hover:text-base-content/60 select-none"
        >
          ⠿
        </span>
        <div className="text-[9.5px] tracking-[0.11em] text-base-content/60 font-semibold flex-1">{title}</div>
        {headerExtra}
      </div>
      <div className="relative mt-2.5">
        <div
          ref={bodyRef}
          onScroll={onBodyScroll}
          onMouseUp={(e) => onResize(id, e.currentTarget.offsetHeight)}
          style={{ height, minHeight: 72 }}
          className="overflow-y-auto resize-y flex flex-col gap-1.5"
        >
          {children}
        </div>
        {overlay}
      </div>
    </div>
  );
}
