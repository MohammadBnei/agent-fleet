import { useCallback, useEffect, useRef, useState } from "react";
import { watchDecisions } from "./connectClient";
import { listOpenDecisions } from "./decisions";
import type { Decision } from "./gen/agentfleet/v1/agent_pb";

// The open decisions, from the decisions store (docs/adr/0082 slice 2).
//
// Under "nothing blocks" a parked decision leaves its session idle, so the old
// liveState === "blocked" derivation missed it. The store is the one place an
// open decision is open whatever the pod is doing. No polling loop: the watch
// stream says when something changed, and every message (the baseline
// included) re-lists, so a dropped message costs nothing but a late list.
//
// `available` goes false for good when AgentService refuses the viewer
// (FLEET_AUTH_DISABLED=1 locally, or no grant): App then falls back to the
// summaries-derived list it has always had.
export function useDecisionDeck(): {
  open: Decision[];
  available: boolean;
  relist: () => void;
} {
  const [open, setOpen] = useState<Decision[]>([]);
  const [available, setAvailable] = useState(true);
  // Lists can overlap (a watch message and a reload). Only the newest one's
  // answer lands, so an older, slower list cannot resurrect an answered row.
  const generation = useRef(0);

  const relist = useCallback(() => {
    const mine = ++generation.current;
    listOpenDecisions()
      .then((decisions) => {
        if (mine === generation.current) setOpen(decisions);
      })
      // A failed list keeps the last one: the stream's next message, or the
      // next reload, retries. Blanking the deck on one bad fetch would close
      // the sheet under someone mid-answer.
      .catch(() => {});
  }, []);

  useEffect(() => {
    // One list up front, apart from the stream: a stream that never delivers
    // (a proxy that breaks server streams, an older core's Unimplemented, a
    // 5xx) would otherwise leave the deck empty while still "available". The
    // stream only keeps it current.
    relist();
    return watchDecisions(relist, () => {
      // Drop any list still in flight, so it cannot refill a dead store.
      generation.current++;
      setAvailable(false);
      setOpen([]);
    });
  }, [relist]);

  return { open, available, relist };
}
