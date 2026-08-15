package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// maxMetricsRange bounds what the Observability page can ask Prometheus
// for. Retention is 7d (infra-bootstrap's prometheus values), so a longer
// window returns nothing useful while still costing a full scan.
const maxMetricsRange = 7 * 24 * time.Hour

// QueryMetrics proxies PromQL to Prometheus. Same error-shape conventions
// as QueryLogs next door.
//
// This is an arbitrary-query endpoint reachable from a browser, so the
// inputs are checked here rather than trusted: it sits behind
// basic-admin-auth and the CSRF header, but "authenticated" is not
// "unbounded".
func (s *Server) QueryMetrics(ctx context.Context, req *connect.Request[agentfleetv1.QueryMetricsRequest]) (*connect.Response[agentfleetv1.QueryMetricsResponse], error) {
	query := strings.TrimSpace(req.Msg.GetQuery())
	if query == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query is required"))
	}
	if s.prom == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("prometheus not configured"))
	}

	startStr, endStr := req.Msg.GetStartTime(), req.Msg.GetEndTime()

	// Both empty is an instant query — the natural shape for a stat tile.
	if startStr == "" && endStr == "" {
		data, err := s.prom.Query(ctx, query)
		if err != nil {
			slog.Error("dashboard QueryMetrics: instant query failed", "query", query, "error", err)
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		return connect.NewResponse(&agentfleetv1.QueryMetricsResponse{ResultJson: string(data)}), nil
	}

	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid start_time: %w", err))
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid end_time: %w", err))
	}
	if !end.After(start) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("end_time must be after start_time"))
	}
	if end.Sub(start) > maxMetricsRange {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("range exceeds %s of retention", maxMetricsRange))
	}

	var step time.Duration
	if raw := req.Msg.GetStep(); raw != "" {
		if step, err = time.ParseDuration(raw); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid step: %w", err))
		}
	}

	data, err := s.prom.QueryRange(ctx, query, start, end, step)
	if err != nil {
		slog.Error("dashboard QueryMetrics: range query failed", "query", query, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.QueryMetricsResponse{ResultJson: string(data)}), nil
}

// GetFleetTopology returns the fleet as cells and the edges between them.
//
// Assembled from what core already owns — the tasks table plus its own
// Prometheus series — and deliberately NOT from the Kubernetes API: core
// holds zero cluster RBAC (docs/adr/0020 point 1), and a topology view is
// not a reason to grant it any. Every session pod's existence is already
// mirrored into tasks.pod_phase by ReportPodEvents, which is the same
// signal the task list renders.
//
// Prometheus being down degrades this rather than failing it: the cells
// still come back, with metrics_error set. Returning zeroed rates silently
// would render an idle-looking fleet that is in fact busy.
func (s *Server) GetFleetTopology(ctx context.Context, _ *connect.Request[agentfleetv1.GetFleetTopologyRequest]) (*connect.Response[agentfleetv1.GetFleetTopologyResponse], error) {
	all, err := s.sessions.List(ctx, defaultListLimit, "")
	if err != nil {
		slog.Error("dashboard GetFleetTopology: list tasks failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	live := make([]sessions.Session, 0, len(all))
	for _, t := range all {
		if sessions.IsPodPhaseLive(t.PodPhase) {
			live = append(live, t)
		}
	}

	nodes := []*agentfleetv1.CellNode{
		{
			Id:     "core",
			Type:   "core",
			Status: "healthy", // it answered this RPC
			Label:  "dispatch · transcript · dashboard",
			Metrics: map[string]float64{
				"live_pods":         float64(len(live)),
				"max_live_sessions": float64(s.maxLive),
			},
		},
		{
			Id:     "provisioner",
			Type:   "provisioner",
			Status: "healthy",
			Label:  "pods · worktrees",
		},
	}

	for _, t := range live {
		nodes = append(nodes, &agentfleetv1.CellNode{
			Id:        "worker/" + t.ID,
			Type:      "worker",
			Status:    cellStatus(t),
			SessionId: t.ID,
			Repo:      t.Repo,
			Label:     t.Description,
		})
	}

	edges := []*agentfleetv1.TopologyEdge{{From: "core", To: "provisioner"}}
	for _, t := range live {
		edges = append(edges, &agentfleetv1.TopologyEdge{From: "provisioner", To: "worker/" + t.ID})
		edges = append(edges, &agentfleetv1.TopologyEdge{From: "worker/" + t.ID, To: "core"})
	}

	resp := &agentfleetv1.GetFleetTopologyResponse{Nodes: nodes, Edges: edges}
	if err := s.annotateRates(ctx, nodes, edges); err != nil {
		slog.Warn("dashboard GetFleetTopology: metrics unavailable", "error", err)
		resp.MetricsError = err.Error()
	}
	return connect.NewResponse(resp), nil
}

// cellStatus maps a task onto the four colours the topology view renders.
// A live pod whose session last recorded an error is `degraded`, not
// `failing` — the pod is still up and the session may well recover; a worker
// that truly died loses its pod phase and drops out of this list entirely.
//
// The `failing` case used to read tasks.status for 'failed'/'failed_permanently'.
// Both are gone (docs/adr/0048), and pod_phase carries the same fact more
// directly: a crashed pod is CRASHED, which is the state a human acts on.
func cellStatus(t sessions.Session) string {
	switch {
	case t.PodPhase != nil && *t.PodPhase == "POD_PHASE_CRASHED":
		return "failing"
	case t.LastError != nil && *t.LastError != "":
		return "degraded"
	case t.PodPhase != nil && *t.PodPhase == "POD_PHASE_RUNNING":
		return "healthy"
	default:
		return "idle" // provisioning/created/scheduled — real, just not working yet
	}
}

// annotateRates fills in the request rates the cells and hub edge carry.
// One instant query for the whole graph, not one per node: the topology
// polls every 5s, and a query per cell would multiply that by the fleet
// size.
func (s *Server) annotateRates(ctx context.Context, nodes []*agentfleetv1.CellNode, edges []*agentfleetv1.TopologyEdge) error {
	if s.prom == nil {
		return errors.New("prometheus not configured")
	}

	coreRate, err := s.scalarQuery(ctx, "sum(rate(agentfleet_grpc_requests_total[5m]))")
	if err != nil {
		return err
	}
	provRate, err := s.scalarQuery(ctx, "sum(rate(agentfleet_provisioner_grpc_requests_total[5m]))")
	if err != nil {
		return err
	}

	for _, n := range nodes {
		switch n.GetId() {
		case "core":
			n.Metrics["rps"] = coreRate
		case "provisioner":
			if n.Metrics == nil {
				n.Metrics = map[string]float64{}
			}
			n.Metrics["rps"] = provRate
		}
	}
	for _, e := range edges {
		if e.GetFrom() == "core" && e.GetTo() == "provisioner" {
			e.Rate = provRate
		}
	}
	return nil
}

// scalarQuery runs an instant query expected to return a single-element
// vector and pulls the number out of it. An empty result is 0, not an
// error: "no samples yet" is the normal state of a freshly restarted core.
func (s *Server) scalarQuery(ctx context.Context, query string) (float64, error) {
	raw, err := s.prom.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	// Prometheus' instant-query `data` is {resultType, result:[{metric,
	// value:[<ts float>, "<value string>"]}]} — the value is a *string*, on
	// purpose, so NaN/Inf survive the JSON round-trip.
	var payload struct {
		Result []struct {
			Value []any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("parse prometheus result: %w", err)
	}
	if len(payload.Result) == 0 || len(payload.Result[0].Value) < 2 {
		return 0, nil
	}
	str, ok := payload.Result[0].Value[1].(string)
	if !ok {
		return 0, nil
	}
	v, err := strconv.ParseFloat(str, 64)
	// NaN and ±Inf are exactly why Prometheus sends the value as a string,
	// and ParseFloat parses all three happily. Letting one through puts a
	// NaN in the JSON the browser gets, where it becomes null and renders
	// as a blank edge — worse than an honest zero.
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, nil
	}
	return v, nil
}
