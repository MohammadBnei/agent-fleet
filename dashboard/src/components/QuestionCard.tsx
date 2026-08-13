import { useEffect, useState } from "react";
import type { TranscriptEntry } from "../gen/agentfleet/v1/transcript_pb";
import { parseQuestions } from "../transcript";

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
  onSubmit,
}: {
  entry: TranscriptEntry;
  answer: Record<string, string> | null;
  busy: boolean;
  compact?: boolean;
  onSubmit: (answers: Record<string, string>) => void;
}) {
  const [selected, setSelected] = useState<Record<number, string[]>>({});
  const [isCollapsed, setIsCollapsed] = useState(false);
  const questions = parseQuestions(entry.text);

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

  const allAnswered = questions.every((_, i) => (selected[i] ?? []).length > 0);

  function submit() {
    if (!questions) return;
    const answers: Record<string, string> = {};
    questions.forEach((q, i) => {
      answers[q.question] = (selected[i] ?? []).join(", ");
    });
    onSubmit(answers);
  }

  return (
    <div className={`border border-pink-line bg-pink-bg ${compact ? "p-3.5" : "p-4 max-w-[760px]"}`}>
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
      {!isCollapsed && (
        <div className={`flex flex-col mt-2.5 ${compact ? "gap-3.5" : "gap-4"}`}>
          {questions.map((q, qIndex) => (
            <fieldset key={qIndex} className="flex flex-col gap-2">
              <legend className="text-base leading-[1.6] text-base-content">
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
                          className={`border cursor-pointer ${
                            compact ? "w-full text-left px-3.5 py-3 text-base" : "px-3.5 py-2 text-sm"
                          } ${
                            active
                              ? "border-primary bg-primary/15 text-primary"
                              : "border-acc-line text-text2 hover:border-primary hover:text-primary"
                          }`}
                        >
                          {opt.label}
                        </button>
                      );
                    })}
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
            </fieldset>
          ))}
          {!answer && (
            <button
              type="button"
              className={`bg-primary text-primary-content font-semibold disabled:opacity-50 ${
                compact ? "w-full py-3 text-base" : "self-start px-5 py-2 text-sm"
              }`}
              disabled={busy || !allAnswered}
              onClick={submit}
            >
              submit answer
            </button>
          )}
        </div>
      )}
    </div>
  );
}
