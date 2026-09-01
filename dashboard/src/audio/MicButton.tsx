import { useCallback, useEffect, useRef, useState } from "react";
import { ConnectError, Code } from "@connectrpc/connect";
import { client } from "../connectClient";

/**
 * Dictation, self-contained.
 *
 * The state and the RPC live here rather than in Composer because Composer is a
 * pure presentational component — value/onChange/onSend, no state, no transport
 * — and an AudioContext plus a gRPC call would invert what it is for. Composer
 * renders this; both the desktop and mobile session views get it for free,
 * since they already share Composer.
 *
 * Audio never leaves as anything but 16 kHz mono s16 PCM, and no STT credential
 * exists in this browser: core proxies to ukubi-stt with its own token and
 * derives the recognizer id from the authenticated identity.
 */
export function MicButton({
  onText,
  disabled,
  compact,
}: {
  onText: (text: string) => void;
  disabled: boolean;
  compact?: boolean;
}) {
  const [recording, setRecording] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const dictationRef = useRef<{ start: () => Promise<void>; stop: () => Promise<void> } | null>(null);
  const streamIdRef = useRef("");

  // Crossing the 640px breakpoint swaps SessionDetail for MobileSessionDetail
  // (App.tsx), which unmounts this. Releasing the microphone on unmount is the
  // part that must not be skipped — the recognizer itself is swept server-side
  // after 120s idle, but a live mic track would otherwise keep the browser's
  // recording indicator on with nothing behind it.
  useEffect(() => {
    return () => {
      void dictationRef.current?.stop();
      dictationRef.current = null;
    };
  }, []);

  // Ahead of the click, in two stages, because the gap between clicking and the
  // graph actually running is speech the user has already said and nothing
  // captured.
  //
  // On mount: pull the code-split chunk down. It holds no resources, so the
  // only cost is a fetch that would otherwise be on the click path.
  // On hover/press: build the context and compile the worklet too. Neither
  // touches the microphone, so this prompts for nothing and lights no recording
  // indicator — which is exactly why it can happen before the user commits.
  const warm = useCallback(() => {
    void import("./stt-capture.js")
      .then((m) => m.prewarm())
      .catch(() => {
        // Warming is an optimisation; start() surfaces the real failure.
      });
  }, []);

  useEffect(() => {
    void import("./stt-capture.js").catch(() => {});
  }, []);

  const stop = useCallback(async () => {
    if (!dictationRef.current || busy) return;
    setRecording(false);
    // stop() flushes the final partial chunk and waits for it to land, so the
    // button is only re-enabled once the last words have actually arrived.
    setBusy(true);
    try {
      await dictationRef.current.stop();
    } finally {
      dictationRef.current = null;
      setBusy(false);
    }
  }, [busy]);

  const start = useCallback(async () => {
    setError(null);
    // Arming is visible: the button reads "…" and is disabled until audio is
    // actually being captured. Even warmed, getUserMedia takes time, and a
    // button that looks ready while the graph is still being built invites
    // exactly the words that then go missing.
    setBusy(true);
    streamIdRef.current = crypto.randomUUID?.() ?? String(Date.now());
    try {
      const { createDictation } = await import("./stt-capture.js");
      const created = createDictation({
        send: async (pcm: Uint8Array, last: boolean) => {
          const res = await client.transcribe({
            audio: pcm,
            streamId: streamIdRef.current,
            last,
            language: "",
          });
          if (res.text) onText(res.text);
        },
        // A failed chunk abandons the dictation instead of retrying it:
        // ordering is load-bearing, so a re-sent chunk arriving after a later
        // one corrupts everything following it.
        onError: (e: Error) => {
          const unavailable = e instanceof ConnectError && e.code === Code.Unavailable;
          setError(unavailable ? "speech-to-text is unavailable" : e.message);
          void stop();
        },
      });
      dictationRef.current = created;
      // Bound to a local before awaiting: the ref can be cleared by unmount or
      // by onError firing during start(), and TS is right that reading it
      // again afterwards is not safe.
      await created.start();
      setRecording(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "microphone unavailable");
      dictationRef.current = null;
    } finally {
      setBusy(false);
    }
  }, [onText, stop]);

  const label = busy ? "…" : recording ? "◼ stop" : "🎙";
  return (
    <button
      type="button"
      disabled={disabled || busy}
      onClick={() => void (recording ? stop() : start())}
      onPointerEnter={warm}
      onPointerDown={warm}
      onFocus={warm}
      title={error ?? (recording ? "Stop dictating" : "Dictate")}
      className={`text-xs flex-none leading-[1.7] disabled:opacity-40 ${
        error ? "text-red-400" : recording ? "text-primary" : "text-dim2"
      } ${compact ? "" : "hover:text-primary cursor-pointer"}`}
    >
      {label}
    </button>
  );
}
