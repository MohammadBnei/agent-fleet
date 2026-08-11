import * as grpc from "@grpc/grpc-js";
import { query, type PermissionResult } from "@anthropic-ai/claude-agent-sdk";
import { ThotServiceService, type ThotServiceServer, type HealthzRequest, type HealthzResponse } from "./gen/agentfleet/v1/thot.js";
import { requestPermission } from "./coreClient.js";
import { SessionQueue } from "./queue.js";

const GRPC_PORT = process.env.GRPC_PORT ?? "9090";
const HTTP_PORT = Number(process.env.HTTP_PORT ?? "8080");
const PERMISSION_TIMEOUT_MS = Number(process.env.THOT_PERMISSION_TIMEOUT_MS ?? "300000");

// Serializes every trigger source onto the one standing session. Exported
// so Phase 4/5/6's entry points (ask_thot, RunAudit, the Alertmanager
// webhook) all funnel through the same instance rather than each inventing
// its own concurrency story.
export const sessionQueue = new SessionQueue();

let sessionReady = false;

// Same shape worker/src/session.ts uses: canUseTool does zero tool
// classification of its own. The SDK's own permissionMode decides *when*
// this is invoked; this callback's only job is "ask a human and block"
// (docs/adr/0029), now against thot_events instead of a task transcript.
async function canUseTool(toolName: string, toolInput: Record<string, unknown>): Promise<PermissionResult> {
  try {
    const decision = await requestPermission(toolName, toolInput, PERMISSION_TIMEOUT_MS);
    if (decision.status === "allowed") {
      return { behavior: "allow", updatedInput: toolInput };
    }
    if (decision.status === "denied") {
      return { behavior: "deny", message: decision.message || "Denied by human." };
    }
    // "pending" == nobody answered within the window. Denying is the only
    // correct move: docs/adr/0029 forbids inferring approval from silence.
    // The request row stays in thot_events, so the dashboard still shows
    // what was asked; thot simply doesn't get to proceed on a non-answer.
    return {
      behavior: "deny",
      message: "No human answered this permission request in time — not proceeding.",
    };
  } catch (err) {
    // Unreachable core is also a denial, never a silent allow.
    console.error("thot canUseTool failed", err);
    return { behavior: "deny", message: `Could not reach core to request permission: ${String(err)}` };
  }
}

async function startSession(): Promise<void> {
  const q = query({
    prompt: (async function* () {
      yield {
        type: "user" as const,
        message: {
          role: "user" as const,
          content:
            "You are thot, a standing cluster diagnostic agent for ukubi-cluster. " +
            "Every mutating action you attempt is gated by a live human approval prompt. " +
            "Wait for work to arrive.",
        },
        parent_tool_use_id: null,
        session_id: "",
      };
      // Streaming-input mode never closes this generator on its own — the
      // process itself is the lifetime boundary for a standing session,
      // unlike worker/'s InputQueue which completes when the task does.
      await new Promise(() => {});
    })(),
    options: {
      executable: "bun",
      model: process.env.THOT_MODEL,
      // CLI parity, same as worker/: the SDK's own mode decides when
      // canUseTool fires. Not "plan" — thot is meant to act, under a gate.
      permissionMode: "default",
      // Deliberately empty: anything listed here bypasses canUseTool
      // entirely (confirmed in worker/'s own Phase 0 spike), which would
      // silently remove the gate rather than add one. Read-only cluster
      // tooling gets added here in Phase 4 only if it's genuinely
      // side-effect-free.
      allowedTools: [],
      settingSources: [],
      canUseTool,
    },
  });

  for await (const msg of q) {
    if (msg.type === "system" && msg.subtype === "init") {
      sessionReady = true;
    }
  }
}

function healthz(
  _call: grpc.ServerUnaryCall<HealthzRequest, HealthzResponse>,
  callback: grpc.sendUnaryData<HealthzResponse>,
): void {
  callback(null, { ok: sessionReady });
}

const serviceImpl: ThotServiceServer = { healthz };

function startGrpcServer(): void {
  const server = new grpc.Server();
  server.addService(ThotServiceService, serviceImpl);
  server.bindAsync(`0.0.0.0:${GRPC_PORT}`, grpc.ServerCredentials.createInsecure(), (err, port) => {
    if (err) {
      console.error("grpc bind failed", err);
      process.exit(1);
    }
    console.log(`thot ThotService listening on :${port}`);
  });
}

function startHttpHealthz(): void {
  Bun.serve({
    port: HTTP_PORT,
    fetch(req) {
      if (new URL(req.url).pathname === "/healthz") {
        return new Response(sessionReady ? "ok" : "starting", { status: sessionReady ? 200 : 503 });
      }
      return new Response("not found", { status: 404 });
    },
  });
  console.log(`thot /healthz listening on :${HTTP_PORT}`);
}

startGrpcServer();
startHttpHealthz();
startSession().catch((err) => {
  console.error("thot session crashed", err);
  process.exit(1);
});
