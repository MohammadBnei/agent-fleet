# Onboard a definition onto the gateway

For [ADR-0088](../adr/0088-fleet-sessions-on-non-claude-models-through-llm-gateway.md). Repo sessions need none of
this: they use the shared `LLM_GATEWAY_KEY_FLEET_CONSOLE`.

A definition can run on a gateway model with **its own key** (steps 1-3 below) or, with no key of its own, on the
**shared definitions key** (`LLM_GATEWAY_KEY_FLEET_DEFINITIONS`, one budget for every definition that has none). Its
model is picked by an admin in the console (Settings, Agents) within the definition's `model.allow`, or set in the file
(step 4). The console offers a gateway model only while core runs with `FLEET_GATEWAY_MODELS=on` and the provisioner
holds either key.

**Shared key, once.** Mint a key as in step 1 and store it as `LLM_GATEWAY_KEY_FLEET_DEFINITIONS` in the same place;
register its hash (step 2) and roll the provisioner (step 3). Then no per-definition key is needed: pick the model in
the console. The provisioner hands the shared key to a definition pod only when core's create request says so, which
core does only for a session on a gateway model; a Claude-model definition pod never holds it.

1. **Mint the key.** In infra-bootstrap, run `gitops/scripts/llm-gateway-new-key.sh` for the definition. The script
   stores it in the project root, so move it in Infisical. The printed block needs `app: agent-fleet` and
   `name: fleet-<definition>` (`fleet-definitions` for the shared key). It belongs in Infisical project `agent-fleet-nygh`, environment `dev`, **folder `/provisioner`** (the `secretsPath` of
   `k8s/provisioner/infisicalsecret.yaml`; a key in the project root is never read) as
   `LLM_GATEWAY_KEY_FLEET_<NAME>`, the definition name upper-cased with
   dashes turned to underscores (`dev-dream-analyst` becomes `LLM_GATEWAY_KEY_FLEET_DEV_DREAM_ANALYST`).
   Set the budget on the key; it is the only limit.
2. **Register its hash.** Infra PR: add the key's hash to the llm-gateway config and to the UI mirror. Merge it and
   let Argo sync llm-gateway.
3. **Roll the provisioner.** It reads the key through `envFrom` once at start, so a new key needs a rollout
   (`kubectl -n agent-fleet rollout restart deploy/provisioner`). A rotation of an existing key needs the
   same rollout; warm pods keep the old key until they are replaced.
4. **Switch the definition.** Either pick the gateway id in the console (Settings, Agents), which refuses it
   until a key exists (it names the missing one), or PR the definition file: `model.default` set to a gateway id (for
   example `kimi-k2-7-code`). Core must run with `FLEET_GATEWAY_MODELS=on`. A console pick applies to the next session
   (a standing one: its next start); a running session keeps its model.
5. **Release the fleet** that carries the change, then start a session and check the transcript model and the
   gateway's per-key spend.

Two name rules follow from that mapping. A definition cannot be named `console` (the repo sessions' shared
key) or `definitions` (the shared definitions key); the definitions loader rejects both. Two names that differ only by dash versus underscore (`a-b`, `a_b`) map to
the same key name; names are dash-only by `nameRe`, so this cannot happen today, but do not loosen that without
revisiting the key names.

A definition with no key still starts: its sidecar answers 503 `fleet gateway key not provisioned for this session`.
To back out, clear the pick in the console or revert the `model.default` PR; the key can stay.
