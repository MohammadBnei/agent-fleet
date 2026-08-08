package lokiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Querier is the interface for querying Loki logs.
// Implemented by *Client and can be mocked in tests.
type Querier interface {
	Query(ctx context.Context, req QueryRequest) ([]LogEntry, error)
}

// Client queries Loki for log entries via its HTTP API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// Ensure Client implements Querier interface.
var _ Querier = (*Client)(nil)

// New creates a Loki client. lokiURL should be the base URL (e.g., http://loki.monitoring.svc.cluster.local:3100).
func New(lokiURL string) *Client {
	return &Client{
		baseURL: lokiURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// QueryRequest specifies filters for log queries.
type QueryRequest struct {
	TaskID    string    // Optional - filter by task-id label
	Namespace string    // "agent-fleet", "default", etc.
	Component string    // "worker", "sidecar", "core", "provisioner", "e2e", "app"
	AppName   string    // Optional - for component="app"
	Level     string    // "debug", "info", "warn", "error" (empty = all)
	Start     time.Time
	End       time.Time
	Limit     int // Max 1000
}

// LogEntry represents a single log entry returned from Loki.
type LogEntry struct {
	Timestamp  time.Time
	Level      string
	Msg        string
	Component  string
	PodName    string
	Namespace  string
	FieldsJSON string // All other slog fields as JSON
}

// Query executes a LogQL query and returns parsed log entries.
func (c *Client) Query(ctx context.Context, req QueryRequest) ([]LogEntry, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("loki not configured (LOKI_URL is empty)")
	}

	// Build LogQL query
	logQL := buildLogQL(req)

	// Build HTTP request
	queryURL := fmt.Sprintf("%s/loki/api/v1/query_range", c.baseURL)
	values := url.Values{}
	values.Set("query", logQL)
	values.Set("start", fmt.Sprintf("%d", req.Start.UnixNano()))
	values.Set("end", fmt.Sprintf("%d", req.End.UnixNano()))

	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	values.Set("limit", fmt.Sprintf("%d", limit))

	httpReq, err := http.NewRequestWithContext(ctx, "GET", queryURL+"?"+values.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("create HTTP request: %w", err)
	}

	// Execute request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("query Loki: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("loki returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	entries, err := parseResponse(body, req.Namespace, req.Component)
	if err != nil {
		return nil, fmt.Errorf("parse Loki response: %w", err)
	}

	return entries, nil
}

// lokiResponse matches Loki's /loki/api/v1/query_range JSON structure.
type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"` // [[timestamp_ns, log_line], ...]
		} `json:"result"`
	} `json:"data"`
}

// parseResponse extracts LogEntry structs from Loki's JSON response.
func parseResponse(body []byte, namespace, component string) ([]LogEntry, error) {
	var resp lokiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal JSON: %w", err)
	}

	if resp.Status != "success" {
		return nil, fmt.Errorf("loki returned status: %s", resp.Status)
	}

	var entries []LogEntry
	for _, result := range resp.Data.Result {
		// Extract labels from stream
		podName := result.Stream["kubernetes_pod_name"]
		if podName == "" {
			podName = result.Stream["pod"]
		}

		for _, value := range result.Values {
			if len(value) != 2 {
				continue
			}

			// Parse timestamp (nanoseconds)
			tsNano := value[0]
			ts, err := parseTimestampNano(tsNano)
			if err != nil {
				continue
			}

			// Parse log line as JSON
			logLine := value[1]
			level, msg, fieldsJSON := parseLogLine(logLine)

			entries = append(entries, LogEntry{
				Timestamp:  ts,
				Level:      level,
				Msg:        msg,
				Component:  component,
				PodName:    podName,
				Namespace:  namespace,
				FieldsJSON: fieldsJSON,
			})
		}
	}

	return entries, nil
}

// parseTimestampNano converts Loki's nanosecond timestamp string to time.Time.
func parseTimestampNano(tsNano string) (time.Time, error) {
	var nanos int64
	if _, err := fmt.Sscanf(tsNano, "%d", &nanos); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, nanos), nil
}
