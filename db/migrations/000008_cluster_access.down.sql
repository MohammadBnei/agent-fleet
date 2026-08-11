DELETE FROM repo_profile_tools WHERE tool_key = 'cluster-access';
DELETE FROM repo_profiles WHERE repo_name = 'infra-bootstrap' AND name = 'worker';
DELETE FROM repos WHERE name = 'infra-bootstrap';
ALTER TABLE repo_profile_tools DROP CONSTRAINT repo_profile_tools_tool_key_check;
ALTER TABLE repo_profile_tools ADD CONSTRAINT repo_profile_tools_tool_key_check
  CHECK (tool_key IN ('go-toolchain', 'bun-toolchain', 'golangci-lint', 'buf'));
