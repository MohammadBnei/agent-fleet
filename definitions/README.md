# definitions/

Defined agents (`docs/adr/0062`). `<name>.yaml` is an `AgentDefinition`,
`clients/<name>.yaml` an `AgentClient`. Any other file here except `*.md` fails
CI. The schema and the fleet ceiling live in `core/internal/definitions`; CI runs
that validator over this directory (`repo_test.go`).

Definitions go live at the next **release tag**, not on merge: the provisioner
reads this directory at its own version (`FLEET_DEFINITIONS_REF` overrides it,
for kind and dev).

## Reviewers (`docs/adr/0082` §9)

`agent-reviewer-<repo>.yaml` is a repo's PR reviewer: `@agent-reviewer` on a PR
of the repo its `credentials` name wakes it. It runs in `git.mode: none`, so its
`forge` key (`GH_` + the vault name, minted by the provisioner) is
read-only, and it posts only through `submit_review`. The `bridge` client must
list it in `definitions`, and its `agent:<name>` in `actors.allow` for its
hand-offs to wake the writer they @mention.

## First responders (`docs/adr/0086` §3)

`alert-responder.yaml` answers Alertmanager alert groups. `trust.untrusted: B` makes a definition a first responder:
single-shot only, `git.mode: none` (no GitHub write), a typed `output.schema` and a `subscribes` naming the alerts it
answers. The `bridge` client must list it. The dispatcher starts one run per alert group (key `alert:<groupKey>`, 10 min
cooldown, held firings coalesced), with the trust set `A·B`; core files its output as a remediation row or a card.
It investigates read-only first: `kubectl_read` (`cluster.read`), `view_logs` (`cluster.logNamespaces`) and
`query_metrics`, all tagged B. Its workspace is `agent-fleet`, since core never runs a definition on a cluster-access repo.
A client's own `Invoke` of a responder is refused. An alert reaches the fleet only through a responder
(`docs/adr/0087`): one no responder subscribes to is acked and files nothing.
It is off for now (owner, 2026-11-23: too much CPU and memory for an answer that cannot close the issue):
the bridge client does not list it and its `invokers` is empty, so every alert is acked and reaches Discord only.

## Characters (`docs/adr/0084`)

`lifecycle: { allow: [standing] }` makes a definition a long-lived character: an admin Starts it from the console,
core keeps its one pod up (Guaranteed QoS from `resources`) until Stop, and it receives only the owner's messages
and trusted hand-offs (set B·C). `ops-lead.yaml` is the first: memory (`memory_write`), reach (`view_logs` over
`cluster.logNamespaces`, `query_metrics`, `list_responder_cards`, `open_issue` on `owns.repos`, each `<owner>/<repo>`) and `spawn_worker` for the one
definition its `spawns.allow` names, `ops-worker.yaml` (a spawn target must be single-shot only and not a first
responder). No client lists a character or its spawn target: Start is the admin's, and `spawns.allow` replaces
`invokers`. `cluster.read` mounts the fleet-read token (infra-bootstrap #462) in the sidecar.

## Model (`docs/adr/0077`)

`model: { default, allow }` is optional; absent, Claude Code picks. Ids come from `definitions.ModelCeiling`: the
`claude-*` ids run on the subscription, the other ids (`zai-org-glm-5`, `z-ai-glm-5-3`, `kimi-k2-7-code`,
`qwen3-coder-480b-a35b-instruct-turbo`, `deepseek-v4-1-flash`) run through the llm-gateway. Core refuses a gateway id
unless `FLEET_GATEWAY_MODELS=on`. No new YAML field: the route is the id.

The `api.anthropic.com` credentials service is required only when the model is absent or a `claude-*` id is in
`default` or `allow`. A definition whose models are all gateway ids needs no vault Anthropic secret; its gateway key is
`LLM_GATEWAY_KEY_FLEET_<DEFINITION_NAME>` (uppercase, dashes to underscores).

## Credentials and onboarding (`docs/adr/0065`)

Every launchable definition names its vault and services in `credentials`; the
`secret` fields are **key names** in Infisical project `agent-vault` folder
`/agent-vault`, which only the provisioner syncs. A missing key fails closed:
the vault reconcile logs it and a warm is refused naming it. The forge is
named, not its host: `forge` is its git (`/<owner>/<repo>/*`,
`/<owner>/<repo>.git/*`) and `forge-api` its API (`/repos/<owner>/<repo>/*`).
Core puts the host `FORGE_URL` names on the vault: `github.com` and
`api.github.com` for `https://github.com`, else that one host with the API
paths under `/api/v1` (Forgejo), and the provisioner refuses any host its own
`FORGE` does not serve. A `forge` / `forge-api` service's key is always `GH_` +
the vault name upper-snaked (`dev-notes` → `GH_DEV_NOTES`), and
`api.anthropic.com` never takes a `GH_` key: the validator and the provisioner
both refuse anything else.

Onboarding a push definition (example: `dev-notes`):

1. Infra-bootstrap's Agent Vault is deployed and bootstrapped
   (`gitops/scripts/agent-vault-bootstrap.sh`).
2. Create a fine-grained PAT scoped to the one repo (contents read/write) and
   store it (`dev-notes` is the exception: it uses a copy of the bot's broad
   `GH_TOKEN`, an accepted risk in ADR 0065) without putting it on argv or in shell history: in the Infisical
   UI, or with infisical's `@file` input from a private temp file:

   ```sh
   f=$(mktemp) && read -rs pat && printf %s "$pat" >"$f" && unset pat &&
     infisical secrets set --projectId=60735f70-38af-46f3-8049-4fc91aabeb68 \
       --env=dev --path=/agent-vault "GH_DEV_NOTES=@$f"; rm -f "$f"
   ```
3. A private repo also needs the provisioner's bot (`argocd-ukubi-bot`) as a
   read collaborator: the clone cache fetches with the bot's token, not the vault.
4. Onboard the repo in the console (manage repos → add), with the URL **exactly**
   `$FORGE_URL/<owner>/<repo>.git` (or without `.git`): the warm refuses
   any other URL for a definition whose credentials name that repo. For
   `dev-notes`: name `notes`, url `https://git.bnei.dev/MohammadBnei/notes.git`,
   base branch `main`, no image, cluster access off.
