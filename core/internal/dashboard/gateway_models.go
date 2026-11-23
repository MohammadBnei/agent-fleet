package dashboard

import "strings"

// gatewayModel is a model served by the llm-gateway instead of the Claude
// subscription: its window and its list price in USD per 1M tokens.
//
// TODO(integration): the model-list slice owns this map, the validModels
// entries and the kill-switch helper; dedupe with it. Prices are unverified.
type gatewayModel struct {
	ContextTokens int64
	InUSD, OutUSD float64
}

var gatewayModels = map[string]gatewayModel{
	"zai-org-glm-5":                        {198000, 1.00, 3.20},
	"z-ai-glm-5-3":                         {1000000, 1.75, 5.50},
	"kimi-k2-7-code":                       {256000, 0.75, 3.50},
	"qwen3-coder-480b-a35b-instruct-turbo": {256000, 0.35, 1.50},
	"deepseek-v4-1-flash":                  {1000000, 0.30, 1.20},
}

// isGatewayRoute: every id that is not a Claude id goes through the gateway.
func isGatewayRoute(id string) bool { return id != "" && !strings.HasPrefix(id, "claude-") }

const errGatewayDisabled = "gateway models are disabled (FLEET_GATEWAY_MODELS)"
