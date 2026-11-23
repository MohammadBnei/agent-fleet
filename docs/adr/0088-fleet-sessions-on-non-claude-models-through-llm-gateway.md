# ADR-0088 — Fleet sessions on non-Claude models through llm-gateway

- **Status:** Accepted (owner, 2026-11-23)
- **Date:** 2026-11-23
- **Amends:**
  - [ADR-0065](0065-agent-vault-and-the-definition-egress-lock.md) §4 (the credential model) and §5 (the egress lock's targets);
  - [ADR-0077](0077-claude-code-picks-the-model.md) (the model list, direct ids, a kill switch);
  - [ADR-0053](0053-the-gate-is-canusetool-not-a-rule-list.md) and [ADR-0061](0061-sessions-launch-in-auto.md) (how `auto` behaves on the gateway route).

## Context

Every session runs on Claude through the subscription OAuth token, which the vault holds. The fleet wants to run
sessions on cheaper non-Claude models (GLM, Kimi, Qwen, DeepSeek) without leaving Claude Code. The cluster already runs
llm-gateway, which speaks the Anthropic Messages API, maps ids to providers and enforces a per-key budget.

## Decision

1. **A gateway model is any model id that does not start with `claude-`.** The first set, with context tokens:
   `zai-org-glm-5` (198000), `z-ai-glm-5-3` (1000000), `kimi-k2-7-code` (256000),
   `qwen3-coder-480b-a35b-instruct-turbo` (256000), `deepseek-v4-1-flash` (1000000). The ids are the gateway's, passed
   straight to Claude Code: no mapping layer in the fleet.
2. **The route is the sidecar.** The worker's `ANTHROPIC_BASE_URL` is `http://127.0.0.1:9094`. The sidecar proxies that
   listener to `FLEET_GATEWAY_URL` (default `http://llm-gateway.llm-gateway.svc.cluster.local:8080`), unbuffered so SSE
   streams. Per request it refuses any `Origin`, drops `Authorization` and `x-api-key`, sets `x-api-key` from the cred
   Secret file `GATEWAY_KEY` (read on every request, so a rotation needs no restart), and overwrites `X-Ukubi-Session`
   and `X-Ukubi-Tag: coding`. Upstream error bodies pass through unchanged. No key: 503 with an Anthropic-format error.
3. **Credential (amends 0065 §4).** The key is long-lived and budget-bounded, minted per definition and one shared key
   for repo sessions (`console`). It lives in Infisical `agent-fleet-nygh` as `LLM_GATEWAY_KEY_FLEET_CONSOLE` or
   `LLM_GATEWAY_KEY_FLEET_<DEFINITION, upper, dashes to underscores>`, reaches the provisioner through `envFrom`, and the
   provisioner writes it to the pod's cred Secret, mounted into the **sidecar only**. Accepted risk: the worker can reach
   the loopback listener and so spend the key, exactly as it can reach the vault token's router. **The budget is the
   only boundary.** The shared console key means one runaway repo session can starve the others; that is accepted for now.
4. **Egress (amends 0065 §5).** The definition egress lock gains llm-gateway pods on port 4000 (Cilium judges after
   the Service DNAT) and the DNS name `llm-gateway.llm-gateway.svc.cluster.local`. Nothing else opens.
5. **Kill switch (amends 0077).** Core's `FLEET_GATEWAY_MODELS=on` enables gateway ids; anything else, the default, is
   off: the model list hides them and create, warm and model switch refuse them with
   "gateway models are disabled (FLEET_GATEWAY_MODELS)".
6. **`auto` on the gateway route (amends 0053, 0061).** The SDK's own `auto` classifier is Claude's and is not trusted
   with another model's output. The route uses the SDK default mode with the fleet's allow: everything runs except
   `FLEET_ASK_RULES`, which park, and `rm`/`sudo`, which deny. A fuller treatment is a later deep dive.
7. **Switching.** Repo sessions only, by a locked teardown and a resume on the new model; the transcript records the
   switch. Definitions change model by a reviewed `model.default` PR. **No fallback:** a gateway failure fails the
   turn, it never retries on Claude, so a cost or data-handling choice is never silently reversed.

## Consequences

- Prices are an input to the stats, to be verified before they are trusted.
- A non-Claude model runs agent loops less reliably; the model's thumbs and scores (a later slice) are how that shows.
- Onboarding a definition is a runbook, not a flag: [gateway-onboard-definition](../runbooks/gateway-onboard-definition.md).
