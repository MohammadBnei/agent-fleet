# Onboard a definition onto the gateway

For [ADR-0088](../adr/0088-fleet-sessions-on-non-claude-models-through-llm-gateway.md). Repo sessions need none of
this: they use the shared `LLM_GATEWAY_KEY_FLEET_CONSOLE`.

1. **Mint the key.** In infra-bootstrap, run `gitops/scripts/llm-gateway-new-key.sh` for the definition. Store the
   key in Infisical project `agent-fleet-nygh` as `LLM_GATEWAY_KEY_FLEET_<NAME>`, the definition name upper-cased with
   dashes turned to underscores (`dev-dream-analyst` becomes `LLM_GATEWAY_KEY_FLEET_DEV_DREAM_ANALYST`).
   Set the budget on the key; it is the only limit.
2. **Register its hash.** Infra PR: add the key's hash to the llm-gateway config and to the UI mirror. Merge it and
   let Argo sync llm-gateway.
3. **Roll the provisioner.** It reads the key through `envFrom` once at start, so a new key needs a rollout
   (`kubectl -n agent-fleet rollout restart deploy/agent-fleet-provisioner`). A rotation of an existing key needs the
   same rollout; warm pods keep the old key until they are replaced.
4. **Switch the definition.** PR the definition file: `model.default` set to a gateway id (for example
   `kimi-k2-7-code`). Core must run with `FLEET_GATEWAY_MODELS=on`.
5. **Release the fleet** that carries the change, then start a session and check the transcript model and the
   gateway's per-key spend.

Two name rules follow from that mapping. A definition cannot be named `console` (that is the repo sessions' shared
key; the definitions loader rejects it). Two names that differ only by dash versus underscore (`a-b`, `a_b`) map to
the same key name; names are dash-only by `nameRe`, so this cannot happen today, but do not loosen that without
revisiting the key names.

A definition with no key still starts: its sidecar answers 503 `fleet gateway key not provisioned for this session`.
To back out, revert the `model.default` PR; the key can stay.
