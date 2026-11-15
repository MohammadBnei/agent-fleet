// Same queue as dashboard/src/serialQueue.ts, copied because the islands image
// builds from an explicit list of dashboard files (core/Dockerfile).
//
// Runs async jobs one at a time, in the order they were queued, and hands each
// caller its own job's result.
//
// Why the console needs it: PostMessage warms a cold session's pod (seconds)
// before it appends, and the server does not order two of them. The second
// call sees the first's PROVISIONING phase, skips the warm and appends at once
// — so it lands BEFORE the first message in the transcript. Rather than lock
// the composer for the length of a warm, sends wait their turn here. One
// failure does not stop the jobs behind it.
export function serialQueue() {
  let tail: Promise<unknown> = Promise.resolve();
  return function enqueue<T>(job: () => Promise<T>): Promise<T> {
    const result = tail.catch(() => {}).then(job);
    tail = result;
    return result;
  };
}
