import { query, type SDKUserMessage, type PermissionResult } from "@anthropic-ai/claude-agent-sdk";
import { requestPermission } from "./coreClient.js";
import { SessionQueue } from "./queue.js";

const PERMISSION_TIMEOUT_MS = Number(process.env.THOT_PERMISSION_TIMEOUT_MS ?? "300000");
const ASK_TIMEOUT_MS = Number(process.env.THOT_ASK_TIMEOUT_MS ?? "600000");

const SYSTEM_PROMPT = `You are thot, the standing cluster agent for ukubi-cluster.

You have kubectl available via Bash, authenticated as your own in-cluster
ServiceAccount. Your RBAC is deliberately bounded: broad read across the
cluster, but only 'patch' on deployments/statefulsets/daemonsets (i.e.
rollout restart) and 'delete' on pods. You cannot read secrets, cannot
touch RBAC objects, and cannot mutate nodes — don't try, and don't
suggest workarounds for those limits.

Every tool call you make is shown to a human for approval before it runs,
so prefer a few precise commands over broad exploratory sweeps.

When answering another agent's question, be concrete: name the failing
object, quote the relevant log or event line, and say what you think the
cause is. If you genuinely can't tell, say so plainly rather than
guessing.`;

// Feeds query()'s streaming-input mode. Same shape as worker/src/session.ts's
// own InputQueue — a standing session needs exactly this, since the
// generator must stay open for the process's whole lifetime.
class InputQueue {
  private queue: SDKUserMessage[] = [];
  private waiters: ((msg: SDKUserMessage) => void)[] = [];
  private sessionId = "";

  setSessionId(id: string): void {
    this.sessionId = id;
  }

  push(text: string): void {
    const msg: SDKUserMessage = {
      type: "user",
      message: { role: "user", content: text },
      parent_tool_use_id: null,
      session_id: this.sessionId,
    };
    const waiter = this.waiters.shift();
    if (waiter) waiter(msg);
    else this.queue.push(msg);
  }

  async *stream(): AsyncIterable<SDKUserMessage> {
    for (;;) {
      const next = this.queue.shift();
      if (next) {
        yield next;
        continue;
      }
      yield await new Promise<SDKUserMessage>((resolve) => this.waiters.push(resolve));
    }
  }
}

// canUseTool does zero tool classification of its own — the SDK's own
// permissionMode decides when it fires; this only asks a human and blocks
// (docs/adr/0029), against thot_events instead of a task transcript.
async function canUseTool(toolName: string, toolInput: Record<string, unknown>): Promise<PermissionResult> {
  try {
    const decision = await requestPermission(toolName, toolInput, PERMISSION_TIMEOUT_MS);
    if (decision.status === "allowed") return { behavior: "allow", updatedInput: toolInput };
    if (decision.status === "denied") {
      return { behavior: "deny", message: decision.message || "Denied by human." };
    }
    // "pending" == nobody answered in time. Denying is the only correct
    // move: adr/0029 forbids inferring approval from silence.
    return { behavior: "deny", message: "No human answered this permission request in time — not proceeding." };
  } catch (err) {
    console.error("thot canUseTool failed", err);
    return { behavior: "deny", message: `Could not reach core to request permission: ${String(err)}` };
  }
}

export class ThotSession {
  #input = new InputQueue();
  #queue = new SessionQueue();
  #ready = false;
  // Resolved by whichever turn is currently in flight; the queue
  // guarantees only one exists at a time.
  #pendingAnswer: ((text: string) => void) | null = null;
  #buffer = "";

  get ready(): boolean {
    return this.#ready;
  }

  get queueDepth(): number {
    return this.#queue.depth;
  }

  /**
   * Puts a prompt to the standing session and resolves with its reply.
   *
   * Serialized through SessionQueue: a query() session processes one turn
   * at a time, so concurrent callers (a worker's ask_thot, a scheduled
   * audit, an alert) would otherwise interleave into one garbled
   * conversation.
   */
  ask(prompt: string, timeoutMs = ASK_TIMEOUT_MS): Promise<string> {
    return this.#queue.enqueue(
      () =>
        new Promise<string>((resolve) => {
          let settled = false;
          const finish = (text: string) => {
            if (settled) return;
            settled = true;
            this.#pendingAnswer = null;
            clearTimeout(timer);
            resolve(text);
          };
          // A turn that never produces a result must not wedge the queue
          // forever behind it — every later caller would hang too.
          const timer = setTimeout(
            () => finish("(thot did not finish answering in time)"),
            timeoutMs,
          );
          this.#buffer = "";
          this.#pendingAnswer = finish;
          this.#input.push(prompt);
        }),
    );
  }

  async run(): Promise<void> {
    const q = query({
      prompt: this.#input.stream(),
      options: {
        executable: "bun",
        model: process.env.THOT_MODEL,
        systemPrompt: SYSTEM_PROMPT,
        // CLI parity. Not "plan" — thot is meant to act, under a gate.
        permissionMode: "default",
        // Deliberately empty: anything listed here bypasses canUseTool
        // entirely (confirmed in worker/'s own Phase 0 spike), so listing
        // Bash would silently remove the gate rather than add one.
        //
        // ponytail: this means even a read-only `kubectl get pods` needs a
        // human click, which is slow for pure diagnosis. That's the
        // deliberate trade-off recorded in ADR-0035 ("always wait for a
        // human, no unattended fallback"). If the clicking becomes
        // genuinely painful, the fix is a narrow read-only MCP tool listed
        // here — NOT putting Bash in this list, which would un-gate
        // mutation too.
        allowedTools: [],
        settingSources: [],
        canUseTool,
      },
    });

    for await (const msg of q) {
      if (msg.type === "system" && msg.subtype === "init") {
        this.#ready = true;
        this.#input.setSessionId(msg.session_id);
      }
      if (msg.type === "assistant") {
        for (const block of msg.message.content) {
          if (block.type === "text") this.#buffer += block.text;
        }
      }
      if (msg.type === "result") {
        this.#pendingAnswer?.(this.#buffer.trim() || "(no answer)");
      }
    }
  }
}
