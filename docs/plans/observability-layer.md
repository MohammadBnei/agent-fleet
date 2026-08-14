# Observability Layer Implementation Plan

## Overview
Add Prometheus metrics + Grafana dashboards + dashboard UI visualization page showing fleet components (core, provisioner, worker pods, e2e pods) as independent cells working together.

## Current State
- **Loki-based logging**: JSON/slog logs aggregated, queryable via LogQL
- **Grafana dashboard exists**: CPU/memory by pod, active workers, error logs (Loki)
- **No Prometheus metrics**: No `/metrics` endpoints, no time-series metrics collection
- **Dashboard UI**: React/Vite/TS/Tailwind, 5 pages (TaskList, TaskDetail, Audits, Worktrees, Files)
- **Components**: core, provisioner, sidecar, worker (Job pods), e2e-runner (sandbox pods)

## Architecture Context
Hub-and-spoke topology:
- **core**: Sole Postgres holder, Discord ingress, dispatch loop, dashboard backend (ConnectRPC + SPA)
- **provisioner**: Only RBAC component, creates worker Jobs + e2e Pods via client-go
- **sidecar**: MCP server in worker pod, telemetry loop (git diff stats every 5s)
- **worker**: TS/Bun, Claude SDK session, single-shot execution
- **e2e-runner**: Sandbox (build/test), code-server, Playwright, per-task pods

## Implementation Steps

### Step 1: Add Prometheus Metrics Endpoints

#### 1.1 Core Metrics (`core/`)
**File**: `core/internal/metrics/metrics.go` (new)

Metrics to expose:
- `agentfleet_tasks_total` (counter, labels: repo, status)
- `agentfleet_tasks_current` (gauge, labels: repo, status, pod_phase)
- `agentfleet_dispatch_claims_total` (counter, labels: outcome=claimed|headroom|retry_cap)
- `agentfleet_transcript_appends_total` (counter, labels: task_id, type, from)
- `agentfleet_pod_lifecycle_events_total` (counter, labels: repo, phase)
- `agentfleet_grpc_requests_total` (counter, labels: method, status)
- `agentfleet_grpc_duration_seconds` (histogram, labels: method)
- `agentfleet_loki_query_duration_seconds` (histogram, labels: component)
- `agentfleet_heartbeat_stale_tasks` (gauge)
- `agentfleet_idle_timeout_candidates` (gauge)

**Integration points**:
- `cmd/core/run.go`: Register `/metrics` handler (port 8080)
- `internal/dispatch/loop.go`: Instrument ClaimNextTask outcomes
- `internal/coreserver/server.go`: Instrument gRPC handlers
- `internal/transcript/store.go`: Instrument appends via activityTrackingStore decorator

#### 1.2 Provisioner Metrics (`provisioner/`)
**File**: `provisioner/internal/metrics/metrics.go` (new)

Metrics:
- `agentfleet_provisioner_pods_created_total` (counter, labels: repo, type=worker|e2e)
- `agentfleet_provisioner_pods_deleted_total` (counter, labels: repo, type)
- `agentfleet_provisioner_git_operations_total` (counter, labels: repo, operation=clone|fetch|worktree_add)
- `agentfleet_provisioner_git_operation_duration_seconds` (histogram, labels: operation)
- `agentfleet_provisioner_grpc_requests_total` (counter, labels: method, status)
- `agentfleet_provisioner_reconcile_duration_seconds` (histogram)

**Integration points**:
- `cmd/provisioner/main.go`: Register `/metrics` handler (port 8080)
- `internal/git/operations.go`: Instrument clone/fetch/worktree operations
- `internal/k8s/pod.go`: Instrument CreateWorkerPod, CreateE2EPod
- `internal/grpcserver/server.go`: Instrument gRPC handlers

#### 1.3 Sidecar Metrics (`sidecar/`)
**File**: `sidecar/internal/metrics/metrics.go` (new)

Metrics:
- `agentfleet_sidecar_mcp_calls_total` (counter, labels: tool_name, status)
- `agentfleet_sidecar_telemetry_pushes_total` (counter, labels: task_id)
- `agentfleet_sidecar_human_messages_delivered_total` (counter, labels: task_id)
- `agentfleet_sidecar_heartbeats_total` (counter, labels: task_id, lease_valid)

**Integration points**:
- `cmd/sidecar/main.go`: Register `/metrics` handler (port 9091 local API)
- `internal/mcp/server.go`: Instrument tool calls
- `internal/telemetry/loop.go`: Instrument telemetry pushes

#### 1.4 Worker SDK Metrics Extraction (`worker/`)
**File**: `worker/src/metrics.ts` (new)

Extract SDK metrics from session results and expose via HTTP endpoint.

**SDK provides** (via `query()` result messages):
- `num_turns`: Total conversation turns
- `total_cost_usd`: Total API cost (notional, subscription mode)
- `duration_ms`: Wall-clock session duration
- `duration_api_ms`: Time in API calls
- `usage.input_tokens`: Context tokens consumed
- `usage.output_tokens`: Tokens generated
- `usage.cache_read_input_tokens`: Cached tokens re-read
- `modelUsage`: Per-model token breakdown
- `permission_denials`: Denied tool calls
- `compact_boundary` events with `pre_tokens` (context size before compaction)

**Expose**:
- Parse result/system messages from `logSdkMessage`
- Accumulate counters/gauges in-process
- Serve `/metrics` on sidecar's LOCAL_API_PORT (9091) alongside existing endpoints
- Metrics reset per worker pod (ephemeral, matches pod lifecycle)

**Metrics to add**:
- `agentfleet_sdk_turns_total` (counter, labels: task_id, model)
- `agentfleet_sdk_cost_usd_total` (counter, labels: task_id, model)
- `agentfleet_sdk_tokens_total` (counter, labels: task_id, model, type=input|output|cache_read)
- `agentfleet_sdk_compactions_total` (counter, labels: task_id)
- `agentfleet_sdk_pre_compact_tokens` (gauge, labels: task_id) - last pre-compaction size
- `agentfleet_sdk_permission_denials_total` (counter, labels: task_id, tool_name)

**Integration**:
- `worker/src/session.ts`: Call `metrics.recordSdkMessage()` in `logSdkMessage`
- `worker/src/index.ts`: Start metrics HTTP server on port 9091 (shares sidecar's port, worker container)
- No Prometheus client lib for Bun yet - hand-write Prometheus text format (trivial: `# TYPE\nmetric{labels} value`)

### Step 2: Prometheus Scrape Configuration

**File**: `k8s/core.yaml` (add ServiceMonitor annotations)

Add to `core` values:
```yaml
podAnnotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8080"
  prometheus.io/path: "/metrics"
```

**File**: `k8s/provisioner/deployment.yaml` (add annotations)
```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8080"
  prometheus.io/path: "/metrics"
```

**Note**: Worker/sidecar pods created dynamically by provisioner need annotations added in `provisioner/internal/k8s/pod.go`

Worker pod annotations (both containers expose metrics):
```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "9091"  # Sidecar LOCAL_API_PORT
prometheus.io/path: "/metrics"
```

### Step 3: Enhanced Grafana Dashboards

**File**: `k8s/core.yaml` (extend dashboards.items)

#### 3.1 Metrics Dashboard (new)
Add `dashboards.items.metrics` with panels:
- Task throughput (rate of tasks created/completed by repo)
- Task latency (time from pending → done by repo)
- Active pods gauge (worker vs e2e, by repo)
- Dispatch claim outcomes (claimed vs headroom vs retry_cap)
- gRPC request rates/errors by method
- Git operation duration percentiles (p50/p95/p99)
- Transcript append rate by type
- Heartbeat/idle timeout drift
- **SDK metrics**: Token usage rate (input/output/cache_read by task)
- **SDK metrics**: Cost accumulation (USD by task)
- **SDK metrics**: Context compactions (frequency, pre-compact token size)
- **SDK metrics**: Permission denials by tool

#### 3.2 Cell Health Dashboard (new)
Add `dashboards.items.cell_health`:
- Per-component uptime/restarts
- Pod lifecycle event rates (created → running → failed)
- Resource saturation (CPU/memory % of limits by component)
- Network errors (gRPC retries, MCP timeouts)
- Reconciliation loop lag

### Step 4: Dashboard UI Visualization Page

#### 4.1 New Page Component
**File**: `dashboard/src/pages/Observability.tsx` (new)

Two views:
1. **Fleet Topology** (default): Real-time cell visualization
2. **Metrics Explorer**: Query builder for Prometheus/Loki

#### 4.2 Fleet Topology View
Interactive diagram showing:
- **Core** (center node): Active gRPC connections, dispatch rate, transcript append rate
- **Provisioner** (left): Git operations in-flight, pod creation queue depth
- **Worker Pods** (right cluster): Per-task cells with live state, CPU/memory sparklines
- **E2E Pods** (bottom): Sandbox status, app ready state, code-server active
- **Flow indicators**: gRPC call rates (edges), MCP tool calls

**Tech stack**:
- D3.js or Cytoscape.js for graph rendering
- Auto-layout (dagre/cola) with manual override
- Color coding: Green (healthy), Yellow (degraded), Red (failing), Gray (idle)
- Click cell → drill into task detail

**Data sources**:
- Prometheus metrics via dashboard backend proxy
- Real-time updates: poll every 5s (reuse existing `pollVisible()` pattern)

#### 4.3 Backend API Extension
**Proto**: `proto/agentfleet/v1/dashboard.proto`

Add RPCs:
```proto
message QueryMetricsRequest {
  string query = 1; // PromQL
  string start_time = 2; // RFC3339
  string end_time = 3;
}

message QueryMetricsResponse {
  string result_json = 1; // Prometheus API response
}

message GetFleetTopologyRequest {}

message GetFleetTopologyResponse {
  message CellNode {
    string id = 1;
    string type = 2; // core|provisioner|worker|e2e
    string status = 3; // healthy|degraded|failing|idle
    map<string, double> metrics = 4; // cpu, memory, request_rate, etc.
  }
  message Edge {
    string from = 1;
    string to = 2;
    double rate = 3; // requests/sec
  }
  repeated CellNode nodes = 1;
  repeated Edge edges = 2;
}

service DashboardService {
  // ... existing RPCs
  rpc QueryMetrics(QueryMetricsRequest) returns (QueryMetricsResponse);
  rpc GetFleetTopology(GetFleetTopologyRequest) returns (GetFleetTopologyResponse);
}
```

**Implementation**: `core/internal/dashboardserver/metrics.go` (new)
- Proxy to Prometheus API (http://platform-prometheus.monitoring.svc.cluster.local:9090)
- Parse Prometheus `/query` and `/query_range` responses
- Aggregate topology from kube-state-metrics + fleet metrics

#### 4.4 Routing Integration
**File**: `dashboard/src/App.tsx`

Add to `View` type:
```ts
export type View = "tasks" | "audits" | "worktrees" | "files" | "observability";
```

Add to `NAV` and `MOBILE_NAV` arrays:
```ts
{ label: "observability", value: "observability" }
```

Render condition:
```tsx
{view === "observability" && <Observability />}
```

### Step 5: Kubernetes Resources Update

#### 5.1 LimitRange Adjustment
**File**: `k8s/core.yaml`

Increase if metrics overhead pushes core over 250m CPU limit (unlikely, but monitor).

#### 5.2 NetworkPolicy
Worker/e2e pods need Prometheus scrape access. Already unrestricted for workers; e2e pods have NetworkPolicy allowing only their worker.

**Fix**: `k8s/provisioner/networkpolicy.yaml` (if needed) – add Prometheus ingress rule.

#### 5.3 Service Discovery
Prometheus auto-discovers annotated pods. No manual ServiceMonitor CRD needed (using prometheus.io annotations, not operator).

### Step 6: Testing & Validation

#### 6.1 Metrics Endpoints
- `curl http://agent-fleet-core.agent-fleet.svc.cluster.local:8080/metrics`
- `curl http://provisioner.agent-fleet.svc.cluster.local:8080/metrics`
- Port-forward sidecar pod, curl `localhost:9091/metrics`

#### 6.2 Prometheus Targets
- Check Prometheus UI (`https://prometheus.bnei.dev/targets`)
- Verify `agentfleet_*` metrics appear in query autocomplete

#### 6.3 Grafana Dashboards
- Verify dashboards auto-load (ConfigMap with `grafana_dashboard: "1"` label)
- Test variable filters (repo, component, task_id)
- Confirm Loki + Prometheus datasource mix works

#### 6.4 Dashboard UI
- Create test task, verify appears in topology view
- Check real-time updates (metrics refresh, state transitions)
- Mobile responsive test (graph reflows to vertical layout)

#### 6.5 E2E Flow
1. Create task via dashboard
2. Watch topology: core → provisioner → worker pod appears
3. Task runs, e2e pod provisions
4. Metrics show: gRPC calls, git operations, MCP tool calls
5. Task completes, pods disappear from topology
6. Verify metrics persist (counters stay, gauges drop)

## Critical Files Modified

### New Files
- `core/internal/metrics/metrics.go`
- `provisioner/internal/metrics/metrics.go`
- `sidecar/internal/metrics/metrics.go`
- `worker/src/metrics.ts` (SDK metrics extraction)
- `core/internal/dashboardserver/metrics.go`
- `dashboard/src/pages/Observability.tsx`
- `dashboard/src/components/FleetTopology.tsx`

### Modified Files
- `core/cmd/core/run.go` (register /metrics)
- `provisioner/cmd/provisioner/main.go` (register /metrics)
- `sidecar/cmd/sidecar/main.go` (register /metrics)
- `worker/src/session.ts` (call metrics.recordSdkMessage in logSdkMessage)
- `worker/src/index.ts` (start metrics HTTP server)
- `k8s/core.yaml` (annotations, dashboards)
- `k8s/provisioner/deployment.yaml` (annotations)
- `provisioner/internal/k8s/pod.go` (pod annotations for worker/sidecar)
- `proto/agentfleet/v1/dashboard.proto` (new RPCs)
- `dashboard/src/App.tsx` (routing)
- `dashboard/package.json` (d3 or cytoscape dependency)

### Dependencies to Add
- **Go**: `github.com/prometheus/client_golang`
- **Worker**: None (hand-write Prometheus text format, no Bun lib exists)
- **Dashboard**: `d3` or `cytoscape` (pick based on interactivity needs)

## Deployment Strategy

### Phase 1: Metrics Infrastructure (Week 1)
1. Add Prometheus client to Go services
2. Implement `/metrics` endpoints
3. Deploy with annotations
4. Verify scraping works

### Phase 2: Grafana Dashboards (Week 1-2)
1. Add metrics dashboard to `k8s/core.yaml`
2. Add cell health dashboard
3. Test panels against live metrics

### Phase 3: Dashboard UI Backend (Week 2)
1. Implement `QueryMetrics` RPC
2. Implement `GetFleetTopology` RPC
3. Test via grpcurl

### Phase 4: Dashboard UI Frontend (Week 2-3)
1. Build Observability page
2. Implement topology view
3. Implement metrics explorer
4. Mobile responsive design

### Phase 5: Integration Testing (Week 3)
1. Full e2e validation
2. Load testing (verify metrics overhead < 5% CPU)
3. Documentation

## Verification Checklist

- [ ] All services expose `/metrics` endpoint
- [ ] Prometheus scrapes all targets (0 errors in /targets)
- [ ] Grafana dashboards load without errors
- [ ] Dashboard UI Observability page renders
- [ ] Topology view shows all active pods
- [ ] Real-time updates work (5s poll)
- [ ] Metrics survive pod restarts (counters recover)
- [ ] Mobile layout works
- [ ] No performance regression (core CPU < 300m under load)
- [ ] Documentation updated (ARCHITECTURE.md, ADR if needed)

## Rollback Plan

1. Remove prometheus annotations from `k8s/` files
2. Remove dashboards from `k8s/core.yaml`
3. Remove Observability page from UI routing
4. Metrics code stays (harmless if not scraped)

## Notes

- **Lazy approach**: No custom exporter pods, direct `/metrics` on each component
- **Reuse existing**: Loki dashboards unchanged, metrics augment not replace
- **Stdlib first**: prometheus/client_golang, no abstractions
- **No extra infra**: Uses existing Prometheus/Grafana in `monitoring` namespace
- **Dashboard UI**: Same tech stack as existing pages, no new frameworks
- **Real-time**: Poll-based (existing pattern), not WebSocket (YAGNI)
