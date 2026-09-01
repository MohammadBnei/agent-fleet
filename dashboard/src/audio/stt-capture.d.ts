// Types for the vendored stt-capture.js, which is plain JS with no build step.
// A sibling .d.ts is how a TS consumer gets a checked surface without either
// side gaining a toolchain.
export interface Dictation {
  start(): Promise<void>;
  /** Flushes the final partial chunk and waits for it to land. */
  stop(): Promise<void>;
  readonly abandoned: boolean;
  readonly CHUNK_SAMPLES: number;
  readonly SAMPLE_RATE: number;
}

export function createDictation(opts: {
  /**
   * Called once per chunk, IN ORDER, never concurrently. Ordering is a
   * contract: the encoder carries cache forward, so a reordered or dropped
   * chunk corrupts everything after it.
   */
  send: (pcm: Uint8Array, last: boolean) => Promise<void>;
  /** A rejecting `send` abandons the dictation and surfaces here. */
  onError?: (err: Error) => void;
  /** 0..1, roughly every 64ms. */
  onLevel?: (level: number) => void;
}): Dictation;

/**
 * Builds the AudioContext and compiles the worklet ahead of the click. Touches
 * no device, so it prompts for nothing and lights no recording indicator —
 * call it on hover. Memoised; safe to call repeatedly.
 */
export function prewarm(): Promise<AudioContext>;

export function toPCM16(samples: Float32Array): Uint8Array;
