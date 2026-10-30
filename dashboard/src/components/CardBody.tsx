import { useState } from "react";
import { agentErrorText } from "../agentErrors";
import { agent } from "../connectClient";
import type { Decision } from "../gen/agentfleet/v1/agent_pb";
import type {
  CardOption,
  CardSubject,
} from "../gen/agentfleet/v1/transcript_pb";
import { relativeTime } from "../relativeTime";
import { ActionButton } from "./ActionButton";
import { InlineError } from "./InlineError";

// A card in the deck (docs/adr/0082): the agent's ask and its options, which
// the owner answers here and nowhere else.
//
// The agent wrote the ask and the labels, so they sit under "Agent claim";
// what an option DOES is core's to say, so core's own effect words stand
// beside each label: a label reading "keep everything" cannot hide a
// close_threads. Nothing is selected up front (ignoring a card never
// approves), and an irreversible option takes a second click on the same
// button, which then names what it is about to do.

type AnswerApi = Pick<typeof agent, "answerDecision">;

const REVERSIBLE: Record<string, string> = {
  yes: "reversible",
  partial: "partly reversible",
  no: "irreversible",
};

function subjectText(s: CardSubject | undefined): string {
  if (!s) return "";
  return s.kind === "repo" ? s.repo : `${s.repo} ${s.kind} #${s.number}`;
}

function effectText(o: CardOption, subject: string): string {
  return o.effect?.verb === "close_threads"
    ? `Closes every thread on ${subject}`
    : "Sends your choice to the agent";
}

// The not-delivered notice. agentsvc owns the reason's words; this only
// names the card, by its subject (the ask can run to 280 characters, too
// long for a banner above the deck).
export function notDeliveredText(
  card: { ask: string; subject?: CardSubject },
  reason: string,
): string {
  const ask = card.ask.length > 60 ? `${card.ask.slice(0, 60)}…` : card.ask;
  return `${subjectText(card.subject) || ask}: not delivered: ${
    reason || "the writer cannot resume"
  }`;
}

// A click on o arms it, rather than sending, when o is irreversible and is
// not the option already armed: the second click on the same button sends.
export function pickArms(o: CardOption, armed: string | null): boolean {
  return o.reversible === "no" && armed !== o.id;
}

export function CardBody({
  decision: d,
  reload,
  onNotDelivered,
  api = agent,
}: {
  decision: Decision;
  reload: () => void;
  // The answer was recorded but the writer cannot resume (its definition
  // moved on, or its session was swept). The deck drops the row on the next
  // list, so the parent keeps the notice where it outlives the row. Omitted
  // (the session feed), the card shows the notice in place of its options.
  onNotDelivered?: (text: string) => void;
  // Tests pass a fake; production omits it.
  api?: AnswerApi;
}) {
  const card = d.card;
  const [armed, setArmed] = useState<string | null>(null);
  const [ownText, setOwnText] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [sent, setSent] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  if (!card) return null;

  const subject = subjectText(card.subject);
  const expires = relativeTime(card.expiresAt, { future: true });

  // `which` is the busy key: an option id, or "own" for the textarea.
  const send = (
    which: string,
    choice: { optionId?: string; ownText?: string },
  ) => {
    setBusy(which);
    setError(null);
    api
      .answerDecision({ decisionId: d.id, ...choice })
      .then((res) => {
        const notice = res.delivered
          ? "sent — the card is answered"
          : notDeliveredText(card, res.notDeliveredReason);
        if (!res.delivered) onNotDelivered?.(notice);
        setSent(notice);
        reload();
      })
      // A busy fleet (no slot to wake the writer) comes back here as an
      // error and the card stays open, so it can be sent again.
      .catch((err: unknown) => setError(agentErrorText(err)))
      .finally(() => setBusy(null));
  };

  const pick = (o: CardOption) => {
    if (pickArms(o, armed)) {
      setArmed(o.id);
      return;
    }
    setArmed(null);
    send(o.id, { optionId: o.id });
  };

  if (sent)
    return (
      // Focusable and focused: the pressed button is gone, and focus must not
      // fall to the page behind the sheet.
      <div
        ref={(el) => el?.focus()}
        tabIndex={-1}
        className="px-4 pt-1 pb-4 text-sm text-dim outline-none"
      >
        {sent}
      </div>
    );

  return (
    <div className="px-4 pt-1 pb-4 flex flex-col gap-3">
      <div className="text-2xs tracking-[0.05em] text-dim2">
        {card.draftedBy === "core" ? "From core" : "Agent claim"}
      </div>
      <div className="text-sm text-text2 [overflow-wrap:anywhere]">
        {card.ask}
      </div>
      <div className="flex flex-wrap gap-x-3 text-xs text-dim2">
        {subject &&
          (card.subject?.url ? (
            <a
              href={card.subject.url}
              target="_blank"
              rel="noreferrer"
              className="hover:text-primary break-all"
            >
              {subject}
            </a>
          ) : (
            <span>{subject}</span>
          ))}
        {expires && <span>expires {expires}</span>}
      </div>
      <InlineError message={error} />
      <div className="flex flex-col gap-2">
        {card.options.map((o) => (
          <ActionButton
            key={o.id}
            busy={busy === o.id}
            disabled={busy !== null}
            keepFocus
            onClick={() => pick(o)}
            className={`text-left cursor-pointer border px-3 py-2 disabled:opacity-50 aria-disabled:opacity-50 ${
              armed === o.id
                ? "border-error text-error"
                : "border-acc-line hover:border-primary"
            }`}
          >
            <span className="flex flex-wrap items-baseline gap-x-2">
              <span className="text-sm font-semibold min-w-0 [overflow-wrap:anywhere]">
                {armed === o.id
                  ? `Confirm: ${o.label}. This cannot be undone`
                  : o.label}
              </span>
              {o.recommended && (
                <span className="text-2xs text-dim2 border border-line px-1">
                  recommended
                </span>
              )}
              <span className="text-2xs text-dim2">
                {REVERSIBLE[o.reversible] ?? o.reversible}
              </span>
            </span>
            <span className="block text-xs text-dim [overflow-wrap:anywhere]">
              {effectText(o, subject)}
            </span>
          </ActionButton>
        ))}
      </div>
      {card.writeOwn && (
        <div className="flex flex-col gap-2">
          <textarea
            value={ownText}
            onChange={(e) => setOwnText(e.target.value)}
            maxLength={2000}
            rows={3}
            placeholder="Or write your own answer"
            aria-label="Write your own answer"
            className="textarea textarea-bordered w-full text-sm"
          />
          <ActionButton
            busy={busy === "own"}
            disabled={busy !== null || ownText.trim() === ""}
            keepFocus
            onClick={() => send("own", { ownText: ownText.trim() })}
            className="self-start cursor-pointer border border-acc-line hover:border-primary px-4 py-1.5 text-sm disabled:opacity-50 aria-disabled:opacity-50"
          >
            Send
          </ActionButton>
        </div>
      )}
    </div>
  );
}
