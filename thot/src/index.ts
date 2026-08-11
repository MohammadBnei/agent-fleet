import * as grpc from "@grpc/grpc-js";
import {
  ThotServiceService,
  type ThotServiceServer,
  type HealthzRequest,
  type HealthzResponse,
  type AskThotRequest,
  type AskThotResponse,
  type RunAuditRequest,
  type RunAuditResponse,
} from "./gen/agentfleet/v1/thot.js";
import { ThotSession } from "./session.js";
import { appendEvent } from "./coreClient.js";

const GRPC_PORT = process.env.GRPC_PORT ?? "9090";
const HTTP_PORT = Number(process.env.HTTP_PORT ?? "8080");
const AUTH_TOKEN = process.env.THOT_AUTH_TOKEN ?? "";

const session = new ThotSession();

// ADR-0035's Consequences called out that the provisioner's
// "network-reachability is the only auth" precedent doesn't scale to a
// component that can mutate the cluster. This is the minimum viable
// step-up: a shared static bearer, checked on every RPC except Healthz.
function authorized(call: { metadata: grpc.Metadata }): boolean {
  if (AUTH_TOKEN === "") return true; // unset == local dev / kind
  const header = call.metadata.get("authorization")[0];
  return typeof header === "string" && header === `Bearer ${AUTH_TOKEN}`;
}

const unauthenticated: grpc.ServiceError = Object.assign(new Error("unauthenticated"), {
  code: grpc.status.UNAUTHENTICATED,
  details: "missing or invalid bearer token",
  metadata: new grpc.Metadata(),
});

function healthz(
  _call: grpc.ServerUnaryCall<HealthzRequest, HealthzResponse>,
  callback: grpc.sendUnaryData<HealthzResponse>,
): void {
  callback(null, { ok: session.ready });
}

function askThot(
  call: grpc.ServerUnaryCall<AskThotRequest, AskThotResponse>,
  callback: grpc.sendUnaryData<AskThotResponse>,
): void {
  if (!authorized(call)) return callback(unauthenticated);

  const { askingTaskId, question } = call.request;
  if (!question) {
    return callback(
      Object.assign(new Error("question is required"), {
        code: grpc.status.INVALID_ARGUMENT,
        details: "question is required",
        metadata: new grpc.Metadata(),
      }) as grpc.ServiceError,
    );
  }

  // The asking task id is context for the answer, not an authorization
  // claim — thot has no task-scoped permissions to check it against.
  const prompt = askingTaskId
    ? `A worker on task ${askingTaskId} asks:\n\n${question}`
    : `A caller asks:\n\n${question}`;

  session
    .ask(prompt)
    .then((answer) => callback(null, { answer }))
    .catch((err) => {
      console.error("askThot failed", err);
      callback(
        Object.assign(new Error(String(err)), {
          code: grpc.status.INTERNAL,
          details: String(err),
          metadata: new grpc.Metadata(),
        }) as grpc.ServiceError,
      );
    });
}

// Fire-and-forget by design: core's audit loop shouldn't block for however
// long an investigation takes (it holds no lease and would just stall the
// scheduler). The response says the audit was accepted; the findings
// themselves land asynchronously in thot_events.
function runAudit(
  call: grpc.ServerUnaryCall<RunAuditRequest, RunAuditResponse>,
  callback: grpc.sendUnaryData<RunAuditResponse>,
): void {
  if (!authorized(call)) return callback(unauthenticated);

  const { auditId, name, prompt } = call.request;
  if (!prompt) {
    return callback(
      Object.assign(new Error("prompt is required"), {
        code: grpc.status.INVALID_ARGUMENT,
        details: "prompt is required",
        metadata: new grpc.Metadata(),
      }) as grpc.ServiceError,
    );
  }

  void session
    .ask(`Scheduled audit "${name}" (${auditId}):\n\n${prompt}`)
    .then((finding) => appendEvent("audit_run", `${name}: ${finding}`, "scheduler"))
    .catch((err) => console.error("audit run failed", name, err));

  callback(null, { status: "queued" });
}

const serviceImpl: ThotServiceServer = { healthz, askThot, runAudit };

function startGrpcServer(): void {
  const server = new grpc.Server();
  server.addService(ThotServiceService, serviceImpl);
  server.bindAsync(`0.0.0.0:${GRPC_PORT}`, grpc.ServerCredentials.createInsecure(), (err, port) => {
    if (err) {
      console.error("grpc bind failed", err);
      process.exit(1);
    }
    console.log(`thot ThotService listening on :${port}`);
    if (AUTH_TOKEN === "") {
      console.warn("THOT_AUTH_TOKEN unset — ThotService is unauthenticated (dev only)");
    }
  });
}

function startHttpHealthz(): void {
  Bun.serve({
    port: HTTP_PORT,
    fetch(req) {
      if (new URL(req.url).pathname === "/healthz") {
        return new Response(session.ready ? "ok" : "starting", { status: session.ready ? 200 : 503 });
      }
      return new Response("not found", { status: 404 });
    },
  });
  console.log(`thot /healthz listening on :${HTTP_PORT}`);
}

startGrpcServer();
startHttpHealthz();
session.run().catch((err) => {
  console.error("thot session crashed", err);
  process.exit(1);
});
