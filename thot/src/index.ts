import * as grpc from "@grpc/grpc-js";
import { query } from "@anthropic-ai/claude-agent-sdk";
import { ThotServiceService, type ThotServiceServer, type HealthzRequest, type HealthzResponse } from "./gen/agentfleet/v1/thot.js";

const GRPC_PORT = process.env.GRPC_PORT ?? "9090";
const HTTP_PORT = Number(process.env.HTTP_PORT ?? "8080");

// Phase 2 scope only: boot the standing session, serve a health check. No
// tools, no canUseTool wiring, no cluster access yet — those land in
// Phase 3 (docs/adr/0035 + the implementation plan's phased build-out).
// Deliberately not "resume:"-based like worker/'s single-shot sessions —
// thot has no prior session to resume on first boot; the resume story for
// a crashed/restarted thot pod is a Phase 3+ concern once there's a real
// session id worth persisting.
let sessionReady = false;

async function startSession(): Promise<void> {
  const q = query({
    prompt: (async function* () {
      yield {
        type: "user" as const,
        message: {
          role: "user" as const,
          content:
            "You are thot, a standing cluster diagnostic agent for ukubi-cluster. " +
            "You have no tools enabled yet — this is a scaffolding boot only. " +
            "Acknowledge and wait.",
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
      permissionMode: "default",
      allowedTools: [],
      settingSources: [],
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
