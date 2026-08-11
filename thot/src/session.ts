import { query, createSdkMcpServer, type SDKUserMessage, type PermissionResult } from "@anthropic-ai/claude-agent-sdk";
import { kubectlReadTool } from "./kubectlRead.js";
import { requestPermission } from "./coreClient.js";
import { SessionQueue } from "./queue.js";

const PERMISSION_TIMEOUT_MS = Number(process.env.THOT_PERMISSION_TIMEOUT_MS ?? "300000");
const ASK_TIMEOUT_MS = Number(process.env.THOT_ASK_TIMEOUT_MS ?? "600000");

const SYSTEM_PROMPT = `You are thot, the standing cluster agent for ukubi-cluster.

Use the kubectl_read tool for ALL diagnosis — it runs read-only kubectl
without interrupting anyone, so reach for it freely and look at as much as
you need. Plain Bash is also available (also kubectl-authenticated), but
every Bash call interrupts a human for approval, so use it only when you
genuinely need to change something. Your RBAC is deliberately bounded: broad read across the
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
        // Read freely, mutate under a gate. kubectl_read is listed here,
        // so it bypasses canUseTool entirely — that's the whole point,
        // since a diagnostic agent that interrupts a human for every
        // `kubectl get pods` is unusable.
        //
        // Bash is deliberately NOT listed: it stays gated, which is what
        // keeps `rollout restart` / `delete pod` blocking on a real human
        // decision (ADR-0035's actual requirement — mutation is gated,
        // reading never had to be). kubectl_read enforces read-only
        // itself; it does not rely on RBAC, which grants patch/delete.
        allowedTools: ["mcp__thot__kubectl_read"],
        settingSources: [],
        mcpServers: {
          thot: createSdkMcpServer({ name: "thot", tools: [kubectlReadTool] }),
        },
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
