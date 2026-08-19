import { useEffect, useState } from "react";
import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { parseQuestions } from "../transcript";
import { ActionButton } from "./ActionButton";
import { TextResponseModal } from "./TextResponseModal";

// One AskUserQuestion batch as a form (already answered: shows what was
// submitted instead). One question per fieldset, chip-style buttons for
// single-select options, checkboxes for multiSelect since a chip can't
// represent "toggle and combine" cleanly.
//
// Desktop and mobile kept separate copies of this and drifted: the mobile
// one silently dropped every option's `description` and the question's
// `header`, so a phone showed bare labels with no explanation of what each
// choice meant — on the one surface where a tooltip can't rescue you.
// `compact` now covers the only real differences (scale, and collapsing
// once answered because a phone feed is short).
export function QuestionCard({
  entry,
  answer,
  busy,
  compact,
  // Drops this card's own border, ground and "QUESTION" label. Set when it is
  // already inside something that says all three — a list row is a pink notch
  // card headed BLOCKED, so the card drew a pink box labelled QUESTION inside a
  // pink box labelled BLOCKED, and the payload lost two levels of indent to
  // chrome that repeated its container.
  embedded,
  onSubmit,
}: {
  entry: TranscriptEntry;
  answer: Record<string, string> | null;
  busy: boolean;
  compact?: boolean;
  embedded?: boolean;
  onSubmit: (answers: Record<string, string>) => void;
}) {
  const [selected, setSelected] = useState<Record<number, string[]>>({});
  const [freeText, setFreeText] = useState<Record<number, string>>({});
  // Which question's free-text modal is open. The draft itself lives inside
  // TextResponseModal, which is what keeps cancel from blanking a saved answer.
  const [editing, setEditing] = useState<number | null>(null);
  const [isCollapsed, setIsCollapsed] = useState(false);
  const questions = parseQuestions(entry.text);

  // One definition for an answer chip, so the free-text button cannot drift
  // from the options it sits beside — it was a smaller, dimmer, differently
  // padded control on the row below them.
  const chip = (active: boolean) =>
    `border cursor-pointer ${compact ? "w-full text-left px-3.5 py-3 text-base" : "px-3.5 py-2 text-sm"} ${
      active
        ? "border-primary bg-primary/15 text-primary"
        : "border-acc-line text-text2 hover:border-primary hover:text-primary"
    }`;

  const typed = (i: number) => Boolean((freeText[i] ?? "").trim());
  const openEditor = (i: number) => setEditing(i);

  // Once answered, a compact feed collapses the card to keep the
  // conversation readable on a small screen.
  useEffect(() => {
    if (compact && answer && !isCollapsed) setIsCollapsed(true);
  }, [compact, answer, isCollapsed]);

  if (!questions) {
    return (
      <div className={`bg-base-300/40 border border-line ${compact ? "px-3 py-2.5" : "px-3 py-2"}`}>
        <div className="text-sm whitespace-pre-wrap">{entry.text}</div>
      </div>
    );
  }

  function toggle(qIndex: number, label: string, multiSelect: boolean | undefined) {
    setSelected((prev) => {
      const current = prev[qIndex] ?? [];
      if (multiSelect) {
        return {
          ...prev,
          [qIndex]: current.includes(label) ? current.filter((l) => l !== label) : [...current, label],
        };
      }
      return { ...prev, [qIndex]: [label] };
    });
  }

  // A question is answered by picking an option OR typing a free-text reply.
  const allAnswered = questions.every((_, i) => (selected[i] ?? []).length > 0 || (freeText[i] ?? "").trim().length > 0);

  function submit() {
    if (!questions) return;
    const answers: Record<string, string> = {};
    questions.forEach((q, i) => {
      // Typed text wins over any selected options — it's the more specific
      // intent when the human bothered to write something.
      answers[q.question] = (freeText[i] ?? "").trim() || (selected[i] ?? []).join(", ");
    });
    onSubmit(answers);
  }

  return (
    <div
      className={
        embedded ? "" : `border border-pink-line bg-pink-bg ${compact ? "p-3.5" : "p-4 max-w-[760px]"}`
      }
    >
      {/* The header is the card's own identity — redundant when embedded, since
          the container already announced it and already carries the pulse dot.
          An ANSWERED card keeps it: that one is a state change worth labelling,
          and it is the collapse control. */}
      {(!embedded || answer) && (
        <button
          type="button"
          onClick={() => compact && answer && setIsCollapsed(!isCollapsed)}
          className="flex items-center gap-2 w-full text-left cursor-pointer disabled:cursor-default"
          disabled={!compact || !answer}
        >
          {!answer && <span className="w-1.5 h-1.5 rounded-full bg-error animate-fpulse" />}
          <span className={`text-2xs tracking-[0.1em] ${answer ? "text-dim2" : "text-error"} flex-1`}>
            {answer ? "ANSWERED" : "QUESTION"}
          </span>
          {compact && answer && <span className="text-2xs text-dim2">{isCollapsed ? "▾" : "▴"}</span>}
        </button>
      )}
      {/* No top margin when embedded — that space existed to clear this card's
          own header, which embedded mode does not render. And a wider gap
          BETWEEN questions than within one (8px), so two questions read as two
          things rather than one long form. */}
      {!isCollapsed && (
        <div className={`flex flex-col ${embedded ? "" : "mt-2.5"} ${compact ? "gap-4" : "gap-5"}`}>
          {questions.map((q, qIndex) => (
            <fieldset key={qIndex} className="flex flex-col gap-2">
              {/* mb on the legend, not a wider fieldset gap: a <legend> is laid
                  out by the fieldset rather than as an ordinary flex child, so
                  the container's `gap` does not separate it from what follows
                  and the question sat flush against its own answer buttons. */}
              <legend className="text-base leading-[1.6] text-base-content mb-3">
                <span className="border border-line text-dim2 text-2xs px-1.5 py-px mr-2">{q.header}</span>
                {q.question}
              </legend>
              {answer ? (
                <div className="self-start border border-acc-line text-primary text-sm px-2 py-0.5">{answer[q.question] ?? "—"}</div>
              ) : q.multiSelect ? (
                q.options.map((opt) => (
                  <label key={opt.label} className="flex items-start gap-2 cursor-pointer text-xs">
                    <input
                      type="checkbox"
                      className="checkbox checkbox-sm mt-0.5"
                      checked={(selected[qIndex] ?? []).includes(opt.label)}
                      onChange={() => toggle(qIndex, opt.label, true)}
                    />
                    <span>
                      <span className="font-medium">{opt.label}</span>
                      {opt.description && <span className="opacity-60"> — {opt.description}</span>}
                    </span>
                  </label>
                ))
              ) : (
                <div className="flex flex-col gap-1.5">
                  <div className={compact ? "flex flex-col gap-2" : "flex flex-wrap gap-2"}>
                    {q.options.map((opt) => {
                      const active = (selected[qIndex] ?? []).includes(opt.label);
                      return (
                        <button
                          key={opt.label}
                          type="button"
                          title={opt.description}
                          onClick={() => toggle(qIndex, opt.label, false)}
                          className={chip(active)}
                        >
                          {opt.label}
                        </button>
                      );
                    })}
                    {/* In the same row and the same shape as the options,
                        because it is the same kind of thing: one more way to
                        answer this question. Set apart only by a dashed border
                        — it opens a dialog rather than selecting a value. */}
                    {!answer && !typed(qIndex) && (
                      <button
                        type="button"
                        onClick={() => openEditor(qIndex)}
                        className={`${chip(false)} border-dashed`}
                      >
                        {q.options.length ? "type something…" : "type a response…"}
                      </button>
                    )}
                  </div>
                  {/* The chips' `title` tooltip is unreachable on a
                      touchscreen, so the selected option's description is
                      shown inline instead of being lost entirely. */}
                  {compact &&
                    q.options
                      .filter((opt) => (selected[qIndex] ?? []).includes(opt.label) && opt.description)
                      .map((opt) => (
                        <div key={opt.label} className="text-xs text-dim2">
                          {opt.description}
                        </div>
                      ))}
                </div>
              )}
              {/* Free-form escape hatch: the human can always type a reply
                  instead of (or as well as) picking an option. Typed text
                  overrides the selection on submit.
                  
                  Behind a button when there are options to pick, because it is
                  the rarer path and an always-open textarea per question is what
                  made a two-question card ~450px tall in a list row — two empty
                  boxes competing with the answers you actually meant to click.
                  With no options it IS the only answer path, so it stays open. */}
              {/* A modal, not an inline reveal. The reveal had no way out:
                  clicking away left an empty box open under the options, and
                  the only dismissal was answering. A dialog closes on Esc, on
                  the backdrop, and on cancel.

                  With text saved this becomes the answer itself plus an edit
                  affordance — otherwise typing something and looking away left
                  no trace that the free-text path had been used at all, while
                  it silently overrode whatever option was selected. */}
              {!answer && typed(qIndex) && (
                  <div className="mt-1 flex items-start gap-2 min-w-0">
                    <button
                      type="button"
                      onClick={() => openEditor(qIndex)}
                      className="flex-1 min-w-0 text-left border border-primary/50 bg-base-300/40 px-2.5 py-1.5 text-sm hover:border-primary cursor-pointer"
                      title="Edit this response"
                    >
                      <span className="block whitespace-pre-wrap break-words">{freeText[qIndex]}</span>
                    </button>
                    <button
                      type="button"
                      onClick={() => setFreeText((prev) => ({ ...prev, [qIndex]: "" }))}
                      aria-label="Clear typed response"
                      title="Clear — falls back to the selected option"
                      className="flex-none text-dim2 hover:text-error px-1 py-1 cursor-pointer"
                    >
                      ✕
                    </button>
                  </div>
              )}
            </fieldset>
          ))}
          {!answer && (
            <ActionButton
              className={`bg-primary text-primary-content font-semibold disabled:opacity-50 ${
                compact ? "w-full py-3 text-base" : "self-start px-5 py-2 text-sm"
              }`}
              busy={busy}
              disabled={!allAnswered}
              onClick={submit}
            >
              submit answer
            </ActionButton>
          )}
        </div>
      )}
      {/* Sibling of the card, not nested inside one of its rows: the editor is
          about the whole answer, and a <dialog> deep inside a flex column
          inherits nothing useful from it. */}
      <TextResponseModal
        open={editing !== null}
        title="YOUR RESPONSE"
        context={editing !== null ? questions[editing]?.question : undefined}
        placeholder="Anything the options do not cover…"
        initial={editing !== null ? (freeText[editing] ?? "") : ""}
        submitLabel="save"
        onSubmit={(text) => editing !== null && setFreeText((prev) => ({ ...prev, [editing]: text }))}
        onClose={() => setEditing(null)}
      />
    </div>
  );
}
