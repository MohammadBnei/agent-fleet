/**
 * Serializes work onto thot's single standing Agent SDK session.
 *
 * A `query()` session processes one turn at a time, but thot has several
 * concurrent trigger sources (worker questions, scheduled audits,
 * Alertmanager alerts). Without this they'd interleave into one garbled
 * conversation.
 *
 * ponytail: a plain in-process FIFO, so a thot restart drops anything
 * queued but not started. Acceptable because every trigger source is
 * already durable or retried at its own layer — an audit re-fires on the
 * next tick, an alert re-fires while still firing, and a worker's
 * ask_thot call gets a gRPC error it can retry. Revisit only if a
 * dropped-on-restart item turns out to matter; a durable queue table is
 * the upgrade path, and it is deliberately not built yet.
 */
export class SessionQueue {
  #tail: Promise<unknown> = Promise.resolve();
  #depth = 0;

  get depth(): number {
    return this.#depth;
  }

  /** Runs fn once every previously-enqueued job has settled. */
  enqueue<T>(fn: () => Promise<T>): Promise<T> {
    this.#depth++;
    // Chain off a catch()'d tail, not the raw one: a rejected job must not
    // poison the chain and reject every job queued behind it.
    const result = this.#tail.then(fn, fn);
    this.#tail = result.then(
      () => undefined,
      () => undefined,
    );
    void result.catch(() => undefined).then(() => {
      this.#depth--;
    });
    return result;
  }
}
