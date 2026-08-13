-- Which environment recipe a repo's e2e sandbox uses (docs/adr/0044).
--
-- Core hardcoded the profile name "e2e" (coreserver.RequestE2EEnv). That is
-- fine for a repo whose recipe happens to be named that, and a dead end for
-- every other: agent-fleet's toolchain lives on its "lint" profile, so the
-- sandbox for the fleet's own repo resolved to no profile at all and came up
-- with none of go/bun/golangci-lint/buf. run_command is registered for every
-- session from turn one (docs/adr/0039), so "no profile named e2e" was not a
-- degradation, it was the sandbox being useless on that repo.
--
-- '' rather than a NOT NULL 'e2e' default, mirroring this same table's
-- base_branch convention ('' means the provisioner defaults to "main"):
-- the fallback stays expressed once, in code, instead of being copied into
-- every existing row by the migration.
ALTER TABLE repos ADD COLUMN e2e_profile TEXT NOT NULL DEFAULT '';

-- agent-fleet's own sandbox: "lint" is where its four toolchains are
-- declared (000003), and its start_cmd is empty, which since 0044 means a
-- sandbox-only pod rather than a failed one — exactly right for a repo whose
-- sandbox exists to build and test, not to serve a preview.
--
-- infra-bootstrap is deliberately NOT pointed at its "worker" profile: that
-- profile carries cluster-access, and granting the sandbox cluster reach
-- would break the less-privileged-than-the-worker premise ADR-0039 rests
-- run_command's un-prompted status on. It falls through to the plain
-- sandbox instead.
UPDATE repos SET e2e_profile = 'lint', updated_at = now() WHERE name = 'agent-fleet';
