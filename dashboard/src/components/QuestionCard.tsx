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
      <div className={`rounded-lg bg-base-200/60 border border-base-content/10 ${compact ? "px-3 py-2.5" : "px-3 py-2"}`}>
        <div className={`${compact ? "text-[12.5px]" : "text-[12px]"} whitespace-pre-wrap`}>{entry.text}</div>
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
    <div
      className={compact ? "rounded-xl border p-3.5" : "rounded-lg border p-4 max-w-[760px]"}
      style={{
        borderColor: "rgba(224,169,78,.45)",
        background: `linear-gradient(180deg,rgba(224,169,78,${compact ? ".1" : ".09"}),rgba(224,169,78,.03))`,
      }}
    >
      <button
        type="button"
        onClick={() => compact && answer && setIsCollapsed(!isCollapsed)}
        className="flex items-center gap-2 w-full text-left"
        disabled={!compact || !answer}
      >
        {!answer && <span className="w-1.5 h-1.5 rounded-full bg-primary animate-fpulse" />}
        <span className={`${compact ? "text-[9px]" : "text-[9.5px]"} tracking-[0.09em] font-semibold text-primary flex-1`}>
          {answer ? "ANSWERED" : "QUESTION"}
        </span>
        {compact && answer && <span className="text-[10px] text-base-content/40">{isCollapsed ? "▾" : "▴"}</span>}
      </button>
      {!isCollapsed && (
        <div className={`flex flex-col mt-2.5 ${compact ? "gap-3.5" : "gap-4"}`}>
          {questions.map((q, qIndex) => (
            <fieldset key={qIndex} className="flex flex-col gap-2">
              <legend className={`${compact ? "text-[12.5px]" : "text-[13px]"} leading-relaxed text-base-content`}>
                <span className="badge badge-ghost badge-sm mr-2">{q.header}</span>
                {q.question}
              </legend>
              {answer ? (
                <div className="badge badge-outline">{answer[q.question] ?? "—"}</div>
              ) : q.multiSelect ? (
                q.options.map((opt) => (
                  <label key={opt.label} className="flex items-start gap-2 cursor-pointer text-[11px]">
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
                  <div className="flex flex-wrap gap-1.5">
                    {q.options.map((opt) => {
                      const active = (selected[qIndex] ?? []).includes(opt.label);
                      return (
                        <button
                          key={opt.label}
                          type="button"
                          title={opt.description}
                          onClick={() => toggle(qIndex, opt.label, false)}
                          className={`px-2.5 py-1 rounded-full text-[10.5px] border cursor-pointer ${
                            active
                              ? "border-primary/60 bg-primary/15 text-primary"
                              : "border-base-content/15 text-base-content/70 hover:border-base-content/30"
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
                        <div key={opt.label} className="text-[10.5px] text-base-content/50">
                          {opt.description}
                        </div>
                      ))}
                </div>
              )}
            </fieldset>
          ))}
          {!answer && (
            <button type="button" className="btn btn-primary btn-sm self-start" disabled={busy || !allAnswered} onClick={submit}>
              Submit answer
            </button>
          )}
        </div>
      )}
    </div>
  );
}
