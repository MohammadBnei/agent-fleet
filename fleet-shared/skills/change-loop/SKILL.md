---
name: change-loop
description: The loop every non-trivial change goes through in ukubi's repos (plan, doubt cycle, proof first, build, checks, independent review, PR), and how a refactor proves it changed nothing. Use before writing code for any feature or refactor, or for a fix that crosses a module, changes a wire format or claims "behaviour unchanged".
---

# The change loop (the ukubi dev rules)

The canonical text is mountain's `plan/16-dev-rules.md`; this is its condensed form. It was written from the ukubi
standard's rollout, and each rule is here because skipping it shipped, or nearly shipped, a bug. The target repo's
own CLAUDE.md wins on its conventions (trailers, test commands, gates).

## The loop
Run every step in order. Skip one only when the human says so.
1. **A short plan.** What changes, what stays. Every claim about today's code carries its `file:line`. List every
   test the change edits and every behaviour it changes.
2. **A doubt cycle** (the `doubt-driven-development` skill). An `Agent` (`Task`) subagent gets the plan and the
   paths, not your conclusions, and is told to disprove the plan. It shares your `/workspace` and can edit it, so its
   brief says to edit nothing and names the uncommitted changes that are yours. It reports numbered findings, each
   with a severity, its evidence and a correction. Take or answer every finding in the plan. A finding that touches a
   decision a human made goes back to that human as a question (`AskUserQuestion`), never around them.
3. **The proof first, from the old code.** Build and run the golden, harness or byte check on the base branch
   before you write the change.
4. **The build,** in the PR shape the plan chose.
5. **Local checks:** the repo's suites and gates, then the proof again. It must be identical, or differ only as the
   plan listed.
6. **An independent review.** A second subagent reviews the diff, with the code around it; brief it the same way.
   Fix every finding, or answer it in the PR with a reason.
7. **The PR:** what changed, the proof and its result, and every test edit and behaviour change, named.

## Planning
- **Check every fact against the code.** Plans have stated the wrong version, a dependency the repo doesn't have,
  and a "status check" that wasn't there.
- **"Behaviour unchanged" and "tests unedited" are claims you prove.** A refactor may edit a test's setup, package
  clauses, imports and names, each listed in the PR; never an assertion. If the design breaks a claim, change the
  design, or drop the claim and label the change.
- **Order is behaviour:** map iteration, sort ties, registry order, the order of log lines and of printed errors.
- **Nil and empty differ on the wire.** `{}` is not `null`: what was always allocated stays allocated.
- **Error text is behaviour** when a log, a client or a test compares it. Keep the strings, the wrapping and the
  decoder.
- **Split and order the PRs.** A fix found during a refactor ships first, apart. A security change ships early. A
  big change is split where each part can be proven alone.
- **Fix the CI a PR needs in its own PR first.** If the fix is under `.github/workflows/`, you can't push it (no
  token carries `workflows`): say so in the PR and ask the human. A CI that is red for a reason outside the PR (an
  unset secret, a runner down) is named in the PR, and the PR is verified locally.
- **Look for consumers outside the repo:** alerts matching a log label, dashboards on a metric name, another repo
  reading a wire field.

## Proving a refactor
- **The proof must reach the changed code.** Break the code on purpose and watch the proof fail. Goldens have
  passed while missing the path entirely: a frames check with no incident in it, and a fixture with no rows for the
  pass being changed.
- **Run the harness twice on the base first.** If it differs from itself it proves nothing. Pin or mask:
  - map iteration (a fail-fast loop over a map);
  - the wall clock (pass a fixed `now`);
  - float sums over maps;
  - more than one failure per case.
- **Drive the real path.** Go through the real client against a local server, not a fake. Record everything the
  path writes: the return, the state, every broadcast, the counters.
- **Methods that worked:**
  - goldens of a transform, compared byte for byte;
  - mutation harnesses for refusals, comparing exit codes and error classes;
  - built output checked byte for byte under every engine and platform CI uses;
  - `go test -v` names before and after;
  - `git diff -M` of moved tests;
  - old and new binaries side by side over every flag and env combination.
- **Every new test gets a mutation:** disable the guarded code (`if false && bad` in Go, `if (false && bad)` in JS)
  and check that the test fails for that reason. A mutation that breaks the build is a false "caught". Every test
  also needs a positive control: a gate test that only checks refusals passes on a gate that refuses everything.
- **Screenshots are not a check.** Compare data, and make sure a trace isn't empty.

## What reviews catch after the tests are green
- **Prose that claims more than the code does,** in most reviews: re-read every doc line, comment and commit message.
- **Error paths:** an early return that dropped the errors already found; a 401 that a page read as "signed out".
- **Unchecked input:** a string spread into letters; a pattern that accepted values off the grid.
- **AST and lint guards with holes:** an alias, a method helper, a write through `slices.Sort`. Give each banned
  form its own mutation.
- **A gap you dismissed** ("unit tests cover it"): deleting the call passed every test.
- **A fake of a service we own,** and a build binary committed by mistake (check `git status`).

## Engines, platforms, tools
- **A comparator that never returns 0 leaves ties in an engine-defined order** (V8 against JavaScriptCore; a stable
  sort doesn't help). Bun's Math is the host's libm. Do an engine-dependent step yourself, in one written-down order.
- **Bun 1.4:**
  - `const` becomes `var`;
  - `--chunk-naming '[name].[ext]'` fails when a chunk and an entry share a name (keep `[hash]`);
  - `bun build` doesn't clean its outdir;
  - a stray `.js` beats its `.ts` when the import names `.js`;
  - `bun test tools` is a substring filter (use `./tools`);
  - `bun add` writes `^` ranges (use `-E`).
- **Git can't see a collision** between two migrations with different names and the same number. Build the merge
  result, not the branch.
- **A tool call that timed out may have worked:** list before you create again.
- Wait on explicit PIDs, never a bare `wait`, and never `pkill -f`.
