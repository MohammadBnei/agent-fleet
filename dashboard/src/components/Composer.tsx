// The message box, shared by SessionDetail and MobileSessionDetail. It was an
// <input> on both — one line, no way to type a newline at all, and a long
// message scrolling sideways inside it. The key handling below is the reason
// this is one component rather than two copies: getting Enter-vs-Shift+Enter
// right involves an IME guard that is easy to write once and easy to forget in
// the second copy.
export function Composer({
  value,
  onChange,
  onSend,
  disabled,
  placeholder,
  compact,
}: {
  value: string;
  onChange: (v: string) => void;
  onSend: () => void;
  disabled: boolean;
  placeholder: string;
  // Mobile: smaller type, a filled ground, and a bare "send" — desktop spells
  // out the keys because it has the room and a keyboard to use them with.
  compact?: boolean;
}) {
  return (
    <div
      className={`flex border border-line px-3 py-2.5 focus-within:border-primary/60 ${
        compact ? "gap-2.5 bg-base-200" : "gap-3"
      } items-end`}
    >
      {/* items-end, not items-center: the box grows downward, and a chevron
          floating in the middle of a three-line message reads as a stray
          glyph rather than a prompt. */}
      <span className="text-primary text-base leading-[1.7] flex-none">❯</span>
      <textarea
        rows={1}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          // isComposing is load-bearing: an IME commits its candidate window
          // with Enter, so without this guard the first half of a composed
          // word gets sent as a message. preventDefault is equally so —
          // without it the newline lands in the box we just cleared.
          if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            onSend();
          }
        }}
        disabled={disabled}
        placeholder={placeholder}
        aria-label="message the agent"
        // field-sizing-content is the browser doing the auto-grow, so there is
        // no ref, no scrollHeight measuring and no effect here. max-h is
        // 3 lines exactly (3 × the 1.7 line-height), after which it scrolls.
        // A browser without field-sizing renders a fixed one-row box that
        // scrolls — degraded, not broken.
        // ponytail: if that ever needs fixing, the upgrade is an onInput
        // handler setting style.height from scrollHeight.
        className={`flex-1 min-w-0 bg-transparent outline-none resize-none field-sizing-content
          leading-[1.7] max-h-[5.1em] overflow-y-auto placeholder:text-dim2 ${
            compact ? "text-sm" : "text-base"
          }`}
      />
      <button
        type="button"
        disabled={disabled || !value.trim()}
        onClick={onSend}
        title="Enter to send, Shift+Enter for a newline"
        className={`text-xs text-dim2 disabled:opacity-40 flex-none leading-[1.7] ${
          compact ? "" : "hover:text-primary disabled:hover:text-dim2 cursor-pointer whitespace-nowrap"
        }`}
      >
        {compact ? "send" : "⏎ send · ⇧⏎ newline"}
      </button>
    </div>
  );
}
