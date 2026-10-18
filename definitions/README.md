# definitions/

Defined agents (`docs/adr/0062`). `<name>.yaml` is an `AgentDefinition`,
`clients/<name>.yaml` an `AgentClient`. Any other file here except `*.md` fails
CI. The schema and the fleet ceiling live in `core/internal/definitions`; CI runs
that validator over this directory (`repo_test.go`).

Definitions go live at the next **release tag**, not on merge: the provisioner
reads this directory at its own version (`FLEET_DEFINITIONS_REF` overrides it,
for kind and dev).
