#!/usr/bin/env bash
#
# check-naming.sh — "safe to publish" as one command with one exit code.
#
# This repository was extracted from a private monorepo. Everything that made it
# private — the originating org and project names, a station's IP, a printer
# serial, an Apple Team ID, lab queue names — has been scrubbed, and this script
# is what keeps it scrubbed. It is a required CI job on every push and PR and a
# blocking pre-push gate, so "the tree is clean" is a checked claim rather than a
# remembered one.
#
# Four assertion classes:
#
#   class 1  naming        the org/project names — zero exceptions, always a
#                          failure
#   class 2  hardware/lab  serials, Team ID, station IP, lab queue names —
#                          zero exceptions, always a failure
#   class 3  workflow      .github/workflows/release.yml's trigger surface, as a
#                          YAML shape check, so a temp dry-run trigger cannot
#                          survive into the default branch
#   class 4  identity      identity.sh <-> identity.go, the go.mod module path,
#                          and the frozen version header
#
# Searching uses `git grep` (tracked files, working-tree content) and never
# `grep -ri`: build artifacts, .git/, and untracked scratch files must not be
# able to produce a phantom hit — nor, more importantly, a phantom pass by
# burying a real one under noise.
#
# Failure output is file, line, and the matched token. Never the whole file: the
# gate runs in public CI logs on a repo whose whole point is that certain strings
# do not appear in public.
#
# Usage:  scripts/check-naming.sh [--fix-hint]
set -euo pipefail

usage() {
	cat <<'USAGE'
usage: scripts/check-naming.sh [--fix-hint]

Verifies that this tree carries no private naming, no hardware or lab artifacts,
no stray release-workflow trigger, and a consistent product identity.

  --fix-hint   on failure, also print how each class is meant to be resolved
  -h, --help   show this message

Exit status: 0 clean, 1 one or more assertions failed, 2 the gate itself could
not run (bad argument, unusable YAML parser).
USAGE
}

FIX_HINT=0
while [ $# -gt 0 ]; do
	case "$1" in
	--fix-hint) FIX_HINT=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		printf 'check-naming.sh: unknown argument: %s\n\n' "$1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

# Anchor on the tree this script lives in, not on the caller's cwd. Every path
# below is repo-relative, and a pre-push hook or a CI step may well invoke the
# gate by absolute path from somewhere else — resolving the root from the caller
# would silently gate a different repository and report it clean.
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)
cd "$REPO_ROOT"

RELEASE_WORKFLOW=".github/workflows/release.yml"
IDENTITY_SH="packaging/identity.sh"
IDENTITY_GO="identity.go"

# The account the public module lives under. The product half of the module path
# is derived from the identity source below rather than restated, so a rename is
# still a one-line edit in packaging/identity.sh.
EXPECTED_MODULE_HOST="github.com/sharaf-nassar"

# Frozen by the wire contract: the SPA's transport reads this header and the
# Browser Print `Device` shape can never grow a version field, so this string is
# the only version channel the product has. It does not get renamed with the
# product.
FROZEN_VERSION_HEADER="X-Print-Agent-Version"

# The banned patterns, each written with its first character bracketed: the
# pattern [f]oo matches the text "foo", while the pattern's own text does not.
# That is deliberate — it means this file spells none of the strings it bans, and
# so it can be scanned by its own gate rather than excluded from it by a
# pathspec. An excluded gate file is a blind spot, and a blind spot in exactly
# the file an attacker or a careless paste would most like to use.
CLASS1_RE='[n]asha|[s]tabletech|[s]table-tech'
CLASS2_RE='D7[N]261550216|YN[T]GW9ST4L|192\.16[8]\.1\.75|[n]asha_zd621|[n]ashaspike'

FAILURES=0
fail() {
	FAILURES=$((FAILURES + 1))
	printf 'FAIL  %s\n' "$*" >&2
}
ok() { printf 'ok    %s\n' "$*"; }
skip() { printf 'skip  %s\n' "$*"; }

# Prints every distinct banned token on one line, comma-separated, in the order
# they appear. The line itself is never printed: this gate's output lands in
# public CI logs, and the tokens are the whole point of the scrub.
tokens_on_line() {
	printf '%s\n' "$2" | grep -oiE "$1" | awk '!seen[$0]++' | paste -sd, - || true
}

# ---------------------------------------------------------------------------
# class 1 — naming
# ---------------------------------------------------------------------------

class1_failures_before=$FAILURES
while IFS= read -r hit; do
	[ -n "$hit" ] || continue
	file=${hit%%:*}
	rest=${hit#*:}
	lineno=${rest%%:*}
	content=${rest#*:}
	token=$(tokens_on_line "$CLASS1_RE" "$content")
	fail "class 1: $file:$lineno: $token (the originating org and project names are never allowed)"
done < <(git grep -In -i -E "$CLASS1_RE" || true)

if [ "$FAILURES" -eq "$class1_failures_before" ]; then
	ok "class 1: private naming — none"
fi

# ---------------------------------------------------------------------------
# class 2 — hardware and lab artifacts
# ---------------------------------------------------------------------------

class2_failures_before=$FAILURES
while IFS= read -r hit; do
	[ -n "$hit" ] || continue
	file=${hit%%:*}
	rest=${hit#*:}
	lineno=${rest%%:*}
	content=${rest#*:}
	token=$(tokens_on_line "$CLASS2_RE" "$content")
	fail "class 2: $file:$lineno: $token (hardware and lab artifacts are never allowed)"
done < <(git grep -In -i -E "$CLASS2_RE" || true)

if [ "$FAILURES" -eq "$class2_failures_before" ]; then
	ok "class 2: hardware and lab artifacts — none"
fi

# ---------------------------------------------------------------------------
# class 3 — release workflow trigger surface
# ---------------------------------------------------------------------------
#
# A YAML shape check, not a grep for the word "push": `push:` nested under a
# `tags:` release trigger is exactly what this repo wants, while `push:` with a
# `branches:` key is the temp trigger used to prove the release workflow on a
# branch — which must never reach the default branch. Because this gate runs on
# every push and PR, leaving that trigger in place reds the very branch that
# would carry it.

if [ ! -f "$RELEASE_WORKFLOW" ]; then
	skip "class 3: $RELEASE_WORKFLOW does not exist yet — trigger surface not checked"
else
	workflow_report=""
	workflow_status=0
	workflow_report=$(python3 - "$RELEASE_WORKFLOW" <<'PY'
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    text = handle.read()

try:
    import yaml
except ImportError:
    sys.stderr.write(
        "check-naming.sh: python3 PyYAML is required for the workflow-shape "
        "assertion (pip install pyyaml)\n"
    )
    sys.exit(2)

MAP = "tag:yaml.org,2002:map"
ALLOWED_TRIGGERS = ("push", "workflow_dispatch")

problems = []


def line_of(node):
    return node.start_mark.line + 1


try:
    root = yaml.compose(text)
except yaml.YAMLError as exc:
    print("%s:1: unparseable YAML (%s)" % (path, exc.__class__.__name__))
    sys.exit(1)

if root is None or root.tag != MAP:
    print("%s:1: workflow is not a YAML mapping" % path)
    sys.exit(1)

# YAML 1.1 resolves a bare `on` key to a boolean; the composer keeps the source
# spelling in .value, so both `on:` and `"on":` land here as the string "on".
triggers_node = None
triggers_line = 1
for key_node, value_node in root.value:
    if str(key_node.value).lower() == "on":
        triggers_node = value_node
        triggers_line = line_of(key_node)
        break

if triggers_node is None:
    print("%s:1: workflow declares no `on:` block" % path)
    sys.exit(1)

if triggers_node.tag != MAP:
    print("%s:%d: `on:` must be a mapping of triggers" % (path, triggers_line))
    sys.exit(1)

push_node = None
push_line = triggers_line
for key_node, value_node in triggers_node.value:
    name = str(key_node.value)
    if name not in ALLOWED_TRIGGERS:
        problems.append((line_of(key_node), name, "trigger is not allowed here"))
        continue
    if name == "push":
        push_node = value_node
        push_line = line_of(key_node)

if push_node is None:
    problems.append((triggers_line, "on", "no `push:` trigger"))
elif push_node.tag != MAP:
    problems.append((push_line, "push", "`push:` must be a mapping with a `tags:` key"))
else:
    keys = [str(k.value) for k, _ in push_node.value]
    for key_node, _ in push_node.value:
        name = str(key_node.value)
        if name != "tags":
            problems.append(
                (line_of(key_node), name, "`push:` may carry only `tags:`")
            )
    if "tags" not in keys:
        problems.append((push_line, "push", "`push:` is missing its `tags:` key"))

for line, token, why in problems:
    print("%s:%d: %s (%s)" % (path, line, token, why))

sys.exit(1 if problems else 0)
PY
	) || workflow_status=$?

	if [ "$workflow_status" -eq 0 ]; then
		ok "class 3: $RELEASE_WORKFLOW triggers are push:tags and workflow_dispatch only"
	elif [ "$workflow_status" -eq 1 ]; then
		while IFS= read -r problem; do
			[ -n "$problem" ] || continue
			fail "class 3: $problem"
		done <<<"$workflow_report"
	else
		printf 'check-naming.sh: could not run the workflow-shape assertion\n' >&2
		exit 2
	fi
fi

# ---------------------------------------------------------------------------
# class 4 — product identity
# ---------------------------------------------------------------------------

identity_failures_before=$FAILURES

shell_binary_name=""
if [ -f "$IDENTITY_SH" ]; then
	shell_binary_name=$(sh -c '. ./'"$IDENTITY_SH"'; printf "%s" "${BINARY_NAME:-}"')
else
	fail "class 4: $IDENTITY_SH: missing (it is the single identity source)"
fi

go_product_name=""
if [ -f "$IDENTITY_GO" ]; then
	go_product_name=$(sed -n 's/^const productName = "\(.*\)"$/\1/p' "$IDENTITY_GO" | head -n 1)
else
	fail "class 4: $IDENTITY_GO: missing (it is the Go half of the identity source)"
fi

if [ -z "$shell_binary_name" ]; then
	fail "class 4: $IDENTITY_SH: BINARY_NAME is unset or empty"
elif [ -z "$go_product_name" ]; then
	fail "class 4: $IDENTITY_GO: productName const not found"
elif [ "$shell_binary_name" != "$go_product_name" ]; then
	fail "class 4: $IDENTITY_GO: productName=$go_product_name != $IDENTITY_SH BINARY_NAME=$shell_binary_name"
fi

module_path=$(sed -n 's/^module[[:space:]]\{1,\}\([^[:space:]]*\).*$/\1/p' go.mod | head -n 1)
if [ -n "$shell_binary_name" ]; then
	expected_module="$EXPECTED_MODULE_HOST/$shell_binary_name"
	if [ "$module_path" != "$expected_module" ]; then
		fail "class 4: go.mod: module $module_path (expected $expected_module)"
	fi
fi

if ! grep -qF "const versionHeader = \"$FROZEN_VERSION_HEADER\"" version.go; then
	fail "class 4: version.go: $FROZEN_VERSION_HEADER is missing or renamed (the header is frozen by the wire contract)"
fi

if [ "$FAILURES" -eq "$identity_failures_before" ]; then
	ok "class 4: identity — $go_product_name, module $module_path, $FROZEN_VERSION_HEADER intact"
fi

# ---------------------------------------------------------------------------

if [ "$FAILURES" -ne 0 ]; then
	printf '\n%s assertion(s) failed — this tree is NOT safe to publish\n' "$FAILURES" >&2
	if [ "$FIX_HINT" -eq 1 ]; then
		cat >&2 <<'HINT'

how each class is meant to be resolved:

  class 1  Rename the string. There is no exception mechanism: this repository
           is independent of the monorepo it was extracted from, and the
           originating org and project names have no reason to reappear in it.
  class 2  Replace the serial, Team ID, station IP, or lab queue name with a
           generic placeholder; documentation examples use https://lab.example.
  class 3  Remove the temporary trigger from .github/workflows/release.yml. The
           release workflow's `on:` block carries push:tags and
           workflow_dispatch, and nothing else.
  class 4  packaging/identity.sh is the authority. Change PRODUCT_NAME there,
           mirror it into identity.go's productName and the go.mod module path,
           and leave the version header alone.
HINT
	fi
	exit 1
fi

printf '\nclean — no private naming, no lab artifacts, identity consistent\n'
exit 0
