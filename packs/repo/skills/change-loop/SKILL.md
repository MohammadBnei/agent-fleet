---
name: change-loop
description: The loop every non-trivial change goes through in ukubi's repos (plan, doubt cycle, proof first, build, checks, independent review, PR), and how a refactor proves it changed nothing. Use before writing code for a feature, a refactor or a fix that crosses a module, changes a wire format or claims "behaviour unchanged".
---

# The change loop (the ukubi dev rules)

Condensed from mountain `plan/16-dev-rules.md` (private; it wins where they differ), written from the ukubi
standard's rollout: 15 doubt cycles and 18 reviews. Each rule is there because skipping it shipped, or nearly
shipped, a bug. The target repo's own CLAUDE.md wins on its conventions (trailers, test commands, gates).

## The loop
1. **A short plan.** What changes, what stays. Every claim about today's code carries its `file:line`. List every
   test the change edits and every behaviour it changes.
2. **A doubt cycle** (the `doubt-driven-development` skill). A `Task` subagent gets the plan and the paths, not
   your conclusions, and is told to disprove the plan. It is read-only. It reports numbered findings, each with a
   severity, its evidence and a correction. Take or answer every finding in the plan. A finding that touches a
   decision a human made goes back to that human as a question, never around them.
3. **The proof first, from the old code.** Build and run the golden, harness or byte check on the base branch
   before you write the change.
4. **The build,** in the PR shape the plan chose.
5. **Local checks:** the repo's suites and gates, then the proof again. It must be identical, or differ only as the
   plan listed.
6. **An independent review.** A second `Task` subagent reviews the diff, with the code around it. Fix every finding,
   or answer it in the PR with a reason.
7. **The PR:** what changed, the proof and its result, and every test edit and behaviour change, named.

## Planning
- **Check every fact against the code.** Plans have stated the wrong version, a dependency the repo doesn't have,
  and a "status check" that wasn't there.
- **"Behaviour unchanged" and "tests unedited" are claims you prove.** A refactor may edit a test's setup, package
  clauses, imports and names, never an assertion. If the design breaks a claim, change the design, or drop the claim
  and label the change.
- **Order is behaviour:** map iteration, sort ties, registry order, the order of log lines and of printed errors.
- **Nil and empty differ on the wire.** `{}` is not `null`: what was always allocated stays allocated.
- **Error text is behaviour** when a log, a client or a test compares it. Keep the strings, the wrapping and the
  decoder.
- **Split and order the PRs.** A fix found during a refactor ships first, apart. A security change ships early. Fix
  the CI a PR needs before the PR. A big change is split where each part can be proven alone.
- **Look for consumers outside the repo:** alerts matching a log label, dashboards on a metric name, another repo
  reading a wire field.

## Proving a refactor
- **The proof must reach the changed code.** Break the code on purpose and watch the proof fail. Goldens have
  passed while missing the path entirely: a frames check with no incident in it, and a fixture that never ran the
  passes.
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
- **Every new test gets a mutation:** disable the guarded code (`|| false &&`), and check that the test fails for
  that reason. A mutation that breaks the build is a false "caught". Every test also needs a positive control: a
  gate test that only checks refusals passes on a gate that refuses everything.
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
- Sorts break ties by engine (V8 against JavaScriptCore), and Bun's Math is the host's libm. Do an
  engine-dependent step yourself, in one order.
- **Bun:**
  - `const` becomes `var`;
  - `--chunk-naming '[name].[ext]'` overwrites the entry;
  - `bun build` doesn't clean its outdir;
  - a stray `.js` beats its `.ts`;
  - `bun test tools` is a substring filter (use `./tools`);
  - `bun add` writes `^` ranges (use `-E`).
- **Git can't see a collision** between two migrations with different names and the same number. Build the merge
  result, not the branch.
- **A tool call that timed out may have worked:** list before you create again.
- Wait on explicit PIDs, never a bare `wait`, and never `pkill -f`.
