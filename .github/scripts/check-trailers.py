#!/usr/bin/env python3
"""Refuse commit messages and PR text that carry the wrong AI attribution.

Usage:
  check-trailers.py [--policy agent|none] [--range BASE..HEAD]

  --range   scan every commit message in BASE..HEAD (two full 40-hex shas)
  PR_TITLE, PR_BODY (environment)
            scan the pull request's title and body when set; they come in
            through the environment, never interpolated into a shell script

Policies:
  agent  (default: ukubi-bridge, ukubi-game, ukubi-kit, agent-fleet) an agent's
         commit may carry
             Co-Authored-By: ukubi-claude-<host> <noreply@bnei.dev>
             Co-Authored-By: ukubi-agent <noreply@bnei.dev>
             Agent-Session: <id>        Agent-Definition: <name>@<sha>
         A human co-author passes. A Co-Authored-By naming Claude or Anthropic
         in any other form is refused.
  none   (mountain) no AI trailer at all: no Co-Authored-By, no Agent-Session.

Both policies refuse a "Claude-Session:" trailer, any other "...-by:" trailer
naming Claude or Anthropic, the Anthropic noreply address anywhere, a
generated-with footer, a claude.ai or claude.com URL and a model name.

A commit's author and committer names are scanned too: GitHub's squash merge
writes a "Co-authored-by:" line on main for every author other than the merger,
so a commit authored as Claude or Anthropic would put the refused trailer on
main even though its message is clean. Only ukubi-claude-<host> and ukubi-agent
at noreply@bnei.dev may be an agent's identity (policy none: no agent at all).

Trailers and the footer are matched at the start of a line after any markdown a
PR body wraps them in (quote, bullet, emphasis). Prints every offending line
with its commit sha (or "PR title" / "PR body") and exits 1 if there is any.
Python 3 standard library only.
"""
import argparse
import os
import re
import subprocess
import sys

I = re.IGNORECASE

# The markdown a trailer or footer may be wrapped in: quote, bullet, emphasis, code.
MD = r"^[\s>*+_`~-]*"
CO_AUTHOR = re.compile(MD + r"co-authored-by\s*:(?P<who>.*)$", I)
# Any other "<something>-by:" trailer (Signed-off-by, Reviewed-by, Assisted-by ...).
BY_TRAILER = re.compile(MD + r"(?!co-authored-by)[a-z][a-z-]*-by\s*:(?P<who>.*)$", I)
ALLOWED_CO_AUTHOR = re.compile(r"^\s*(ukubi-claude-[A-Za-z0-9._-]+|ukubi-agent)\s+<noreply@bnei\.dev>\s*$")
# "ukubi-claude-<host>" and env names such as CLAUDE_MODEL are not model names:
# a model name is claude-<family> or <family> <number>.
FAMILY = r"(?:opus|sonnet|haiku|fable)"
SEP = r"[ _\u2010-\u2015-]"  # space, underscore, hyphen, and the unicode hyphens and dashes
MODEL = re.compile(
    r"(?<![A-Za-z0-9])(?<!ukubi-)claude" + SEP + r"(?:\d[\w.-]*" + SEP + r")?(?:" + FAMILY + r"|instant)(?![A-Za-z])"
    r"|(?<![A-Za-z0-9])(?<!ukubi-)claude[_\u2010-\u2015-]\d"
    r"|(?<![A-Za-z0-9])" + FAMILY + SEP + r"?\d",
    I,
)


# A rule is (name, policies it applies to, function(line) -> bool). Add a rule
# here; nothing else changes.
def _co_author_any(line):
    return bool(CO_AUTHOR.match(line))


def _names_ai(who):
    """who (the text after a trailer's colon) names Claude or Anthropic, and is not an allowed agent."""
    who = who.lstrip(" \t*_`~")  # the closing emphasis of "**Co-Authored-By:**"
    if ALLOWED_CO_AUTHOR.match(who):
        return False
    return bool(re.search(r"claude|anthropic", who, I))


def _co_author_ai(line):
    m = CO_AUTHOR.match(line)
    return bool(m) and _names_ai(m.group("who"))


def _by_trailer_ai(line):
    m = BY_TRAILER.match(line)
    return bool(m) and _names_ai(m.group("who"))


RULES = [
    ("Co-Authored-By (no AI trailer allowed here)", {"none"}, _co_author_any),
    ("Co-Authored-By names Claude or Anthropic", {"agent"}, _co_author_ai),
    ("Agent-Session (no AI trailer allowed here)", {"none"}, lambda l: bool(re.match(MD + r"agent-session\s*:", l, I))),
    ("Claude-Session trailer", {"agent", "none"}, lambda l: bool(re.match(MD + r"claude-session\s*:", l, I))),
    ("trailer names Claude or Anthropic", {"agent", "none"}, _by_trailer_ai),
    ("Anthropic noreply address", {"agent", "none"}, lambda l: bool(re.search(r"noreply@anthropic\.com", l, I))),
    # The footer is a line that starts with it; a sentence that says "regenerated with buf" or "code
    # generated with buf generate" is codegen talk. "generated with/by Claude" is refused anywhere.
    ("generated-with footer", {"agent", "none"}, lambda l: bool(re.search(r"^[\W_]*generated\s+(?:with|by)\b", l, I))),
    ("generated with Claude", {"agent", "none"}, lambda l: bool(re.search(r"\bgenerated\s+(?:with|by)\s+claude\b", l, I))),
    ("claude.ai / claude.com URL", {"agent", "none"}, lambda l: bool(re.search(r"(?<![A-Za-z0-9-])claude\.(?:ai|com)(?![A-Za-z0-9-])", l, I))),
    ("model name", {"agent", "none"}, lambda l: bool(MODEL.search(l))),
]


def offences(text, policy):
    """Yield (rule name, line) for every offending line of one message."""
    for line in text.split("\n"):
        for name, policies, hit in RULES:
            if policy in policies and hit(line):
                yield name, line


def identity_offence(who, policy):
    """The rule an author or committer identity breaks, or None."""
    if policy == "none":
        return "AI identity (no agent allowed here)" if re.search(r"claude|anthropic|ukubi-agent", who, I) else None
    return "author or committer names Claude or Anthropic" if _names_ai(who) else None


def commits(rng):
    base, _, head = rng.partition("..")
    for sha in (base, head):
        if not re.fullmatch(r"[0-9a-f]{40}", sha):
            sys.exit("check-trailers: --range wants two full 40-hex shas, got %r" % rng)
    out = subprocess.run(
        ["git", "log", "--format=%H%x00%an <%ae>%x00%cn <%ce>%x00%B%x1e", "%s..%s" % (base, head), "--"],
        check=True, stdout=subprocess.PIPE,
    ).stdout.decode("utf-8", "replace")
    for rec in out.split("\x1e"):
        rec = rec.strip("\n")
        if rec:
            sha, author, committer, body = rec.split("\x00", 3)
            yield sha[:12], author, committer, body


def show(line):
    # One physical line, no control characters: a log line must not be able to
    # start a workflow command.
    line = "".join(c if c.isprintable() else "?" for c in line.strip())
    return line if len(line) <= 200 else line[:200] + "..."


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--policy", choices=["agent", "none"], default="agent")
    ap.add_argument("--range")
    args = ap.parse_args(argv)

    messages = []
    identities = []  # (where, "Name <email>") of every author and committer in the range
    if args.range:
        for sha, author, committer, body in commits(args.range):
            messages.append((sha, body))
            identities += [(sha + " author", author), (sha + " committer", committer)]
    for label, var in (("PR title", "PR_TITLE"), ("PR body", "PR_BODY")):
        if os.environ.get(var):
            messages.append((label, os.environ[var]))
    if not args.range and not messages:
        sys.exit("check-trailers: nothing to scan (give --range and/or PR_TITLE / PR_BODY)")

    bad = 0
    for where, who in identities:
        name = identity_offence(who, args.policy)
        if name:
            bad += 1
            print("%s: %s: %s" % (where, name, show(who)))
    for where, text in messages:
        for name, line in offences(text, args.policy):
            bad += 1
            print("%s: %s: %s" % (where, name, show(line)))
    if bad:
        print("check-trailers: %d offending line(s) (policy %s)" % (bad, args.policy))
        return 1
    print("check-trailers: %d message(s), %d identities clean (policy %s)" % (len(messages), len(identities), args.policy))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
