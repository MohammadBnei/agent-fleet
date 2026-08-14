// Package promclient queries Prometheus over its HTTP API, the same shape
// lokiclient uses for Loki. core proxies these queries for the dashboard's
// Observability page rather than letting the browser reach Prometheus
// directly: Prometheus has no IngressRoute in this cluster (only Grafana and
// Alertmanager do), so it is unreachable from a browser by construction.
//
// This is a query client, not cluster access — core still holds zero RBAC
// (docs/adr/0020 point 1), and reading a time series is not reading the
// Kubernetes API.
package promclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Querier is what the dashboard server depends on, so tests can substitute
// a fake without an HTTP server — same contract as lokiclient.Querier.
type Querier interface {
	Query(ctx context.Context, q string) (json.RawMessage, error)
	QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) (json.RawMessage, error)
}

type Client struct {
	baseURL    string
	httpClient *http.Client
}

var _ Querier = (*Client)(nil)

// New builds a client for a Prometheus base URL, e.g.
// http://platform-prometheus-kube-p-prometheus.monitoring.svc.cluster.local:9090
func New(promURL string) *Client {
	return &Client{
		baseURL: promURL,
		httpClient: &http.Client{
			// Same 30s ceiling lokiclient uses. A PromQL query that takes
			// longer than this is one the dashboard shouldn't be running.
			Timeout: 30 * time.Second,
		},
	}
}

// Query runs an instant query and returns Prometheus' raw JSON `data`
// envelope. Deliberately unparsed: the dashboard renders Prometheus' own
// result shapes (vector/matrix/scalar), and re-modelling them in proto would
// buy nothing but a second thing to keep in sync.
func (c *Client) Query(ctx context.Context, q string) (json.RawMessage, error) {
	return c.get(ctx, "/api/v1/query", url.Values{"query": {q}})
}

// QueryRange runs a range query. step is clamped so a wide range can't ask
// Prometheus for an unbounded number of points.
func (c *Client) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) (json.RawMessage, error) {
	if step <= 0 {
		step = time.Minute
	}
	// Prometheus rejects queries over 11,000 points outright; picking a
	// coarser step beats handing the user that error.
	if points := end.Sub(start) / step; points > 10000 {
		step = end.Sub(start) / 10000
	}
	return c.get(ctx, "/api/v1/query_range", url.Values{
		"query": {q},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	})
}

func (c *Client) get(ctx context.Context, path string, values url.Values) (json.RawMessage, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("prometheus not configured (PROMETHEUS_URL is empty)")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+values.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query prometheus: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	// Prometheus answers a bad query with 400 and a JSON body carrying the
	// real reason ("parse error at char 7: ..."). Surfacing that beats
	// "status 400", since the query came from a human typing PromQL.
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("prometheus returned status %d: %s", resp.StatusCode, string(body))
	}
	if envelope.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", envelope.Error)
	}
	return envelope.Data, nil
}
