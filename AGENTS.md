# Agent Instructions

`browser-print-agentd` is a **Go 1.24 + macOS packaging** project. It is a single flat
`package main` at the repository root plus a `packaging/` tree that builds a signed, notarized
`.pkg` installer. There is no frontend, no database, no service backend, and no design system —
if a task description implies one, the description is wrong.

Read `README.md` before touching anything: it states the frozen wire contract, the install
layout, and the release chain. Read `lat.md/` for the design intent behind the code.

## Repository shape

| Path                        | What it is                                                                       |
| --------------------------- | -------------------------------------------------------------------------------- |
| `*.go` (repo root)          | the whole agent — flat `package main`, stdlib only, no `go.sum`                  |
| `agent_test.go`             | the unit suite, including the CUPS-parsing fixtures captured from real hardware  |
| `packaging/identity.sh`     | **the single identity source**: product name, bundle id, binary name, dir names  |
| `packaging/*.in`            | templates rendered from `identity.sh` by `build-pkg.sh` — never edit the output  |
| `packaging/build-pkg.sh`    | cross-compile → stage → `pkgbuild` → `productbuild` → optional `productsign`     |
| `scripts/check-naming.sh`   | the repository hygiene gate                                                      |
| `lat.md/`                   | the design/architecture knowledge graph                                          |

## Commands

```bash
go build ./...                    # build
go vet ./...                      # vet
go test -race ./...               # unit tests, race detector on
scripts/check-naming.sh           # repository hygiene gate — exit 0/1
packaging/build-pkg.sh --stage-only   # verify the installer layout, no macOS needed
lat check                         # validate lat.md wiki links and code refs
```

`agent_test.go` is the entire test surface. There is no Python suite, no pytest, and no vendored
harness in this repository.

Before handing work back, `go build ./...`, `go vet ./...`, `go test -race ./...`,
`scripts/check-naming.sh`, and `lat check` must all be green.

## Hard constraints

- **The wire contract is frozen.** `/available`, `/default`, `/write`, `/read`, `/health`, their
  request and response shapes, status codes, plain-text error bodies, the CORS origin echo, the
  1500 ms `/available` probe budget, and byte-exact `^GFA` passthrough do not change. `/default`
  returns an **empty body** — not `{}` — when nothing is healthy.
- **`X-Print-Agent-Version` is a fixed header name**, and the `Device` object must never grow a
  version field. `check-naming.sh` asserts both.
- **Identity lives in one place.** Change `packaging/identity.sh` (and `identity.go`'s
  `productName` in lockstep), never a literal in a plist, `distribution.xml`, an install script,
  or a workflow. Every packaging artifact is a `.in` template.
- **The release workflow stays tag-only.** No `push: branches:` trigger may reach `main`;
  `check-naming.sh` fails the build if one does.
- **No lab-specific strings.** No originating-monorepo names, no printer serials, no station IP
  addresses, no site-specific queue names, no Team ID. Documentation examples use
  `https://lab.example`. `check-naming.sh` enforces this with **no allowlist and no exception
  mechanism**; fix a hit by renaming the string, never by widening the gate.
- **New tests require an explicit request** and must be anchored to a section in
  `lat.md/tests.md` with a matching `// @lat:` code ref.
- **Signing and notarization credentials never appear in a file, a log, a command line, or a
  commit.** They are GitHub secrets, set interactively with `gh secret set`.

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation
prompts. `cp`, `mv`, and `rm` may be aliased to `-i` on some systems, which makes an agent hang
forever waiting for y/n.

```bash
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

Other commands that may prompt: `scp` and `ssh` (`-o BatchMode=yes`), `apt-get` (`-y`), `brew`
(`HOMEBREW_NO_AUTO_UPDATE=1`).

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->

<!-- BEGIN BEADS CODEX SETUP: generated by bd setup codex -->
## Beads Issue Tracker

Use Beads (`bd`) for durable task tracking in repositories that include it. Use the `beads` skill at `.agents/skills/beads/SKILL.md` (project install) or `~/.agents/skills/beads/SKILL.md` (global install) for Beads workflow guidance, then use the `bd` CLI for issue operations.

### Quick Reference

```bash
bd ready                # Find available work
bd show <id>            # View issue details
bd update <id> --claim  # Claim work
bd close <id>           # Complete work
bd prime                # Refresh Beads context
```

### Rules

- Use `bd` for all task tracking; do not create markdown TODO lists.
- Run `bd prime` when Beads context is missing or stale. Codex 0.129.0+ can load Beads context automatically through native hooks; use `/hooks` to inspect or toggle them.
- Keep persistent project memory in Beads via `bd remember`; do not create ad hoc memory files.

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.
<!-- END BEADS CODEX SETUP -->
