package models

import (
	"math"
	"testing"
)

func TestRouteAndGateway(t *testing.T) {
	for _, c := range []struct {
		id, route string
		ctx       int32
	}{
		{"", RouteSubscription, 0},
		{"claude-opus-5", RouteSubscription, 0},
		{"zai-org-glm-5", RouteGateway, 198000},
		{"z-ai-glm-5-3", RouteGateway, 1000000},
		{"kimi-k2-7-code", RouteGateway, 256000},
		{"qwen3-coder-480b-a35b-instruct-turbo", RouteGateway, 256000},
		{"deepseek-v4-1-flash", RouteGateway, 1000000},
	} {
		if got := Route(c.id); got != c.route {
			t.Errorf("Route(%q) = %s, want %s", c.id, got, c.route)
		}
		if i, ok := Gateway(c.id); ok != (c.route == RouteGateway) || i.ContextTokens != c.ctx {
			t.Errorf("Gateway(%q) = %+v, %v", c.id, i, ok)
		}
	}
	for _, id := range IDs() {
		if Route(id) != RouteGateway {
			t.Errorf("%s in the map but routes to the subscription", id)
		}
	}
}

func TestBilledUSD(t *testing.T) {
	if got := BilledUSD("zai-org-glm-5", 1_000_000, 500_000, 0); math.Abs(got-2.6) > 1e-9 {
		t.Errorf("glm-5 = %v, want 2.6", got)
	}
	if BilledUSD("claude-opus-5", 1e6, 1e6, 1e6) != 0 {
		t.Error("subscription must bill 0")
	}
}
