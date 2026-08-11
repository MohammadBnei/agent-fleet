import * as grpc from "@grpc/grpc-js";
import { CoreServiceClient } from "./gen/agentfleet/v1/core.js";

// thot's one outbound gRPC connection to core. Mirrors
// sidecar/internal/coreclient's shape: a single long-lived client, plain
// insecure credentials (in-cluster only, gated by NetworkPolicy), and
// waitForReady so a core restart is a retry rather than a crash.
//
// docs/adr/0035: thot reaches core for *persistence* like every other
// component — core stays the sole Postgres-credential holder. The
// hub-and-spoke exception in that ADR is about callers reaching thot
// directly, not about thot bypassing core.
const CORE_GRPC_ADDR = process.env.CORE_GRPC_ADDR ?? "agent-fleet-core.agent-fleet.svc.cluster.local:9090";

const client = new CoreServiceClient(CORE_GRPC_ADDR, grpc.credentials.createInsecure());

// grpc-js has no (request, options, callback) overload — call options only
// come after a Metadata argument, so every call passes an empty one.
function deadline(ms: number): Partial<grpc.CallOptions> {
  return { deadline: Date.now() + ms };
}

const noMetadata = () => new grpc.Metadata();

export type PermissionDecision =
  | { status: "allowed" }
  | { status: "denied"; message: string }
  | { status: "pending"; requestId: string };

/**
 * Appends a permission_request and blocks until a human decides, or the
 * server-side long-poll times out.
 *
 * A timeout returns "pending" — never "allowed". Silence is not consent
 * (docs/adr/0029); the caller must treat pending as not-yet-approved.
 */
export function requestPermission(
  toolName: string,
  input: unknown,
  timeoutMs = 60_000,
): Promise<PermissionDecision> {
  return new Promise((resolve, reject) => {
    client.requestThotPermission(
      { toolName, inputJson: JSON.stringify(input ?? {}), timeoutMs },
      noMetadata(),
      // Client deadline must outlast the server's own long-poll window,
      // or every prompt dies client-side before a human can ever answer.
      deadline(timeoutMs + 15_000),
      (err, res) => {
        if (err) return reject(err);
        if (res.status === "allowed") return resolve({ status: "allowed" });
        if (res.status === "denied") return resolve({ status: "denied", message: res.message });
        resolve({ status: "pending", requestId: String(res.requestId) });
      },
    );
  });
}

export function appendEvent(
  kind: "finding" | "alert" | "audit_run",
  payload: string,
  actor = "thot",
  idempotencyKey = "",
): Promise<void> {
  return new Promise((resolve, reject) => {
    client.appendThotEvent({ kind, actor, payload, idempotencyKey }, noMetadata(), deadline(10_000), (err) => {
      if (err) return reject(err);
      resolve();
    });
  });
}
