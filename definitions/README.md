# definitions/

Defined agents (`docs/adr/0062`). `<name>.yaml` is an `AgentDefinition`,
`clients/<name>.yaml` an `AgentClient`. Any other file here except `*.md` fails
CI. The schema and the fleet ceiling live in `core/internal/definitions`; CI runs
that validator over this directory (`repo_test.go`).

Definitions go live at the next **release tag**, not on merge: the provisioner
reads this directory at its own version (`FLEET_DEFINITIONS_REF` overrides it,
for kind and dev).

## Credentials and onboarding (`docs/adr/0065`)

Every launchable definition names its vault and services in `credentials`; the
`secret` fields are **key names** in Infisical `agent-fleet-nygh` folder
`/agent-vault`, which only the provisioner syncs. A missing key fails closed:
the vault reconcile logs it and a warm is refused naming it.

Onboarding a push definition (example: `dev-notes`):

1. Infra-bootstrap's Agent Vault is deployed and bootstrapped
   (`gitops/scripts/agent-vault-bootstrap.sh`).
2. Create a fine-grained PAT scoped to the one repo (contents read/write) and
   store it: `infisical secrets set --projectId=ae771c2c-5115-452a-8f1c-1e03fa0e2b9a --env=dev --path=/agent-vault GH_DEV_NOTES=<pat>`.
3. A private repo also needs the provisioner's bot (`argocd-ukubi-bot`) as a
   read collaborator: the clone cache fetches with the bot's token, not the vault.
4. Onboard the repo in the console (manage repos → add), with the URL **exactly**
   `https://github.com/<owner>/<repo>.git` (or without `.git`): the warm refuses
   any other URL for a definition whose credentials name that repo. For
   `dev-notes`: name `notes`, url `https://github.com/MohammadBnei/notes.git`,
   base branch `main`, no image, cluster access off.
