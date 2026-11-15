// Runs async jobs one at a time, in the order they were queued, and hands each
// caller its own job's result.
//
// Why the console needs it: PostMessage warms a cold session's pod (seconds)
// before it appends, and the server does not order two of them. The second
// call sees the first's PROVISIONING phase, skips the warm and appends at once
// — so it lands BEFORE the first message in the transcript. Rather than lock
// the composer for the length of a warm, sends wait their turn here. One
// failure does not stop the jobs behind it.
export type Enqueue = ReturnType<typeof serialQueue>;

export function serialQueue() {
  let tail: Promise<unknown> = Promise.resolve();
  return function enqueue<T>(job: () => Promise<T>): Promise<T> {
    const result = tail.catch(() => {}).then(job);
    tail = result;
    return result;
  };
}

// One queue per key (a session id), kept for the life of the page: leaving a
// session and coming back, or a layout switch remounting its view, must land
// on the SAME queue, or a send made before the switch races one made after.
const queues = new Map<string, Enqueue>();
export function queueFor(key: string): Enqueue {
  let q = queues.get(key);
  if (!q) {
    q = serialQueue();
    queues.set(key, q);
  }
  return q;
}
