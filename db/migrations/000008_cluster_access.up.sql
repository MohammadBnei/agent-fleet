-- Seeds everything a thot session needs to dispatch as an ordinary worker
-- task (docs/adr/0037).
--
-- The point of this migration is that the dispatch path needs NO code
-- change: core's dispatch loop and warmIfIdle both call repos.Get and
-- abort on a miss, so giving thot a real repos row is what lets a
-- cluster-agent session flow through the existing machinery untouched.

-- 1. Allow the new tool key. The catalog<->CHECK pairing is guarded by
--    provisioner/internal/catalog/catalog_test.go, which failed loudly
--    when the catalog gained cluster-access before this ran.
ALTER TABLE repo_profile_tools DROP CONSTRAINT repo_profile_tools_tool_key_check;
ALTER TABLE repo_profile_tools ADD CONSTRAINT repo_profile_tools_tool_key_check
  CHECK (tool_key IN
    ('go-toolchain', 'bun-toolchain', 'golangci-lint', 'buf', 'cluster-access'));

-- 2. thot sessions are worker tasks on infra-bootstrap — the repo that
--    actually holds the cluster's own IaC, so a durable fix is a normal
--    PR against it.
INSERT INTO repos (name, url, base_branch)
VALUES ('infra-bootstrap', 'https://github.com/MohammadBnei/infra-bootstrap.git', 'main')
ON CONFLICT (name) DO NOTHING;

-- 3. The "worker" profile for that repo grants cluster-access, which is
--    what installs the kubectl shim. Deliberately data, not code: which
--    tasks may reach the cluster is dashboard-editable (docs/adr/0028's
--    no-redeploy principle), and no other repo's profile has it.
INSERT INTO repo_profiles (repo_name, name)
VALUES ('infra-bootstrap', 'worker')
ON CONFLICT (repo_name, name) DO NOTHING;

INSERT INTO repo_profile_tools (profile_id, tool_key)
SELECT id, 'cluster-access' FROM repo_profiles
WHERE repo_name = 'infra-bootstrap' AND name = 'worker'
ON CONFLICT DO NOTHING;
