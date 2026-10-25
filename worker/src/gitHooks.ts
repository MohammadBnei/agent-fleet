// The co-author trailer a repo session's commits carry; a definition pod's
// carry none (its git identity already names the agent). Both kinds also get
// Agent-Session, and a definition pod Agent-Definition (worker/git-hooks).
export const FLEET_TRAILER = "Co-Authored-By: ukubi-agent <noreply@bnei.dev>";

// worker/git-hooks, as worker/Dockerfile copies it. Its commit-msg rewrites
// whatever trailer the model wrote into FLEET_TRAILER (or nothing, in a
// definition pod), then runs the repo's own hook of the same name.
const FLEET_GIT_HOOKS = process.env.FLEET_GIT_HOOKS ?? "/app/worker/git-hooks";

// core.hooksPath for every git the agent runs, appended to whatever
// GIT_CONFIG_* the provisioner already set (a definition pod's safe.directory)
// rather than replacing it. Env, not a gitconfig file: a definition pod has no
// $HOME git setup at all (worker/src/index.ts).
//
// ponytail: `git -c core.hooksPath= commit` or `--no-verify` skips it; the
// definition `tools` that could deny those are inert until P4.
export function gitHooksEnv(env: Record<string, string | undefined>): Record<string, string> {
  const n = Number(env.GIT_CONFIG_COUNT) || 0;
  return {
    GIT_CONFIG_COUNT: String(n + 1),
    [`GIT_CONFIG_KEY_${n}`]: "core.hooksPath",
    [`GIT_CONFIG_VALUE_${n}`]: FLEET_GIT_HOOKS,
  };
}
