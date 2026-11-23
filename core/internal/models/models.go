// Package models is the one place that knows which model ids run through the
// LLM gateway instead of the subscription (docs/adr/0065 amendment). The
// route is the id: anything not starting with "claude-" is a gateway id.
// Prices are USD per 1M tokens, to be verified against the provider.
package models

import "strings"

// Info is a gateway model's context window and price.
type Info struct {
	ContextTokens int32
	InUSD, OutUSD float64 // per 1M tokens
}

const (
	RouteSubscription = "subscription"
	RouteGateway      = "gateway"
	// DisabledMsg is what every refusal says when FLEET_GATEWAY_MODELS is not "on".
	DisabledMsg = "gateway models are disabled (FLEET_GATEWAY_MODELS)"
)

var gateway = map[string]Info{
	"zai-org-glm-5":                        {198000, 1.00, 3.20},
	"z-ai-glm-5-3":                         {1000000, 1.75, 5.50},
	"kimi-k2-7-code":                       {256000, 0.75, 3.50},
	"qwen3-coder-480b-a35b-instruct-turbo": {256000, 0.35, 1.50},
	"deepseek-v4-1-flash":                  {1000000, 0.30, 1.20},
}

// Gateway returns the gateway model's info; ok is false for any other id.
func Gateway(id string) (Info, bool) {
	i, ok := gateway[id]
	return i, ok
}

// IDs lists the gateway model ids (unsorted).
func IDs() []string {
	out := make([]string, 0, len(gateway))
	for id := range gateway {
		out = append(out, id)
	}
	return out
}

// Route is "gateway" for an id that is not a claude-* id, else "subscription".
// "" (no model: Claude Code picks) is the subscription.
func Route(id string) string {
	if id != "" && !strings.HasPrefix(id, "claude-") {
		return RouteGateway
	}
	return RouteSubscription
}

// BilledUSD prices tokens on the gateway map; 0 for a subscription or unknown id.
// Cache tokens count as input: gateway models report no cache discount.
func BilledUSD(id string, in, out, cache int64) float64 {
	i, ok := Gateway(id)
	if !ok {
		return 0
	}
	return float64(in+cache)/1e6*i.InUSD + float64(out)/1e6*i.OutUSD
}
