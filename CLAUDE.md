# browser-print-agentd

A **Go 1.24 + macOS packaging** project: a localhost HTTP print agent that emulates the Zebra
Browser Print wire contract on top of CUPS, plus the `packaging/` tree that builds its signed and
notarized `.pkg` installer.

There is no frontend, no database, no service backend, and no design system in this repository.
The whole agent is a flat `package main` at the repository root with a stdlib-only dependency
set — there is no `go.sum` and no vendor directory.

Read `README.md` for the frozen wire contract and the install layout. The release chain lives in
`RUNBOOK.md` and `lat.md/infrastructure.md`.

# Repository shape

| Path                      | What it is                                                                           |
| ------------------------- | ------------------------------------------------------------------------------------ |
| `*.go` (repo root)        | the whole agent — flat `package main`, stdlib only, no `go.sum`                      |
| `agent_test.go`           | the unit suite, including the CUPS-parsing fixtures captured from real hardware      |
| `packaging/identity.sh`   | **the single identity source**: product name, bundle id, binary name, dir names      |
| `packaging/*.in`          | templates rendered from `identity.sh` by `build-pkg.sh` — never edit the output      |
| `packaging/build-pkg.sh`  | cross-compile → stage → `pkgbuild` → `productbuild` → optional `productsign`         |
| `scripts/check-naming.sh` | the repository hygiene gate                                                          |
| `README.md`               | what the agent **is**: plain-language intro, install, the frozen wire contract        |
| `RUNBOOK.md`              | what an admin **does** to a station: install, migrate, roll back, diagnose, validate |
| `lat.md/`                 | **why** it behaves that way — the design/architecture knowledge graph                |

# Before starting work

- Run `lat search "<what you are about to change>"` to find the sections that describe the design
  intent, and read them before writing code.
- Run `lat expand` on user prompts to expand any `[[refs]]` — this resolves section names to file
  locations and provides context.

# Commands

```bash
go build ./...                        # build
go vet ./...                          # vet
go test -race ./...                   # unit tests, race detector on
scripts/check-naming.sh               # repository hygiene gate — exit 0/1
packaging/build-pkg.sh --stage-only   # verify the installer layout, no macOS needed
packaging/build-pkg.sh --version X.Y.Z  # full package build (macOS only)
lat check                             # validate lat.md wiki links and code refs
```

`agent_test.go` is the entire test surface. There is no Python suite, no pytest, and no vendored
harness in this repository.

`packaging/build-pkg.sh` also reads `VERSION`, `ARCH`, `OUTPUT_DIR`, `APP_SIGNING_IDENTITY`,
`INSTALLER_SIGNING_IDENTITY`, `STAGE_ONLY`, and `GO_LDFLAGS` from the environment.

# Hard constraints

- **The wire contract is frozen.** `/available`, `/default`, `/write`, `/read`, `/health`, their
  request and response shapes, status codes, plain-text error bodies, the CORS origin echo, the
  1500 ms `/available` probe budget, and byte-exact `^GFA` passthrough do not change. `/default`
  returns an **empty body** — not `{}` — when nothing is healthy.
- **`X-Print-Agent-Version` is a fixed header name**, and the `Device` object must never grow a
  version field.
- **Identity lives in one place**: `packaging/identity.sh`, mirrored by `identity.go`'s
  `productName`. Every packaging artifact is a `.in` template rendered from it — never edit a
  literal bundle id, label, binary name, or install path anywhere else.
- **The release workflow stays tag-only.** A `push: branches:` trigger must never reach `main`.
- **No lab-specific strings**: no originating-monorepo names, printer serials, station IPs,
  site-specific queue names, or Team IDs. Examples use `https://lab.example`.
  `scripts/check-naming.sh` enforces this with **no allowlist and no exception mechanism** — fix
  a hit by renaming the string, never by widening the gate.
- **New tests require an explicit request**, and must be anchored to a `lat.md/tests.md` section
  with a matching `// @lat:` code ref.
- **Credentials never land in a file, a log, a command line, or a commit.** Apple signing and
  notarization values are GitHub secrets set interactively with `gh secret set`.

# Post-task checklist (REQUIRED — do not skip)

After EVERY task, before responding:

- [ ] `go build ./...`, `go vet ./...`, `go test -race ./...` green
- [ ] `scripts/check-naming.sh` green
- [ ] Update `lat.md/` if you added or changed functionality, architecture, tests, or behavior
- [ ] `lat check` green — all wiki links and code refs must pass
- [ ] End your reply with the direct answer to the user's request. Keep any lat.md sync note to
      ONE short trailing line — never end on sync commentary, a drift table, or a checklist.

---

# What is lat.md?

This project uses [lat.md](https://www.npmjs.com/package/lat.md) to maintain a structured
knowledge graph of its architecture, design decisions, and test specs in the `lat.md/` directory.
It is a set of cross-linked markdown files that describe **what** this project does and **why** —
the domain concepts, key design decisions, and test specifications. Use it to ground your work in
the actual architecture rather than guessing.

```bash
lat locate "Section Name"      # find a section by name (exact, fuzzy)
lat refs "file#Section"        # find what references a section
lat search "natural language"  # semantic search across all sections
lat expand "user prompt text"  # expand [[refs]] to resolved locations
lat check                      # validate all links and code refs
```

Run `lat --help` when in doubt about available commands or options.

If `lat search` fails because no API key is configured, explain that semantic search requires a
key provided via `LAT_LLM_KEY` (direct value), `LAT_LLM_KEY_FILE` (path to key file), or
`LAT_LLM_KEY_HELPER` (command that prints the key). Supported key prefixes: `sk-...` (OpenAI) or
`vck_...` (Vercel). If the user doesn't want to set it up, use `lat locate` for direct lookups.

# Syntax primer

- **Section ids**: `lat.md/path/to/file#Heading#SubHeading` — full form uses project-root-relative
  path (e.g. `lat.md/tests#Agent Core`). Short form uses the bare file name when it is
  unique (e.g. `tools#CUPS Contract`).
- **Wiki links**: `[[target]]` or `[[target|alias]]` — cross-references between sections. They can
  also reference source code: `[[server.go#agent#ServeHTTP]]`.
- **Source code links**: wiki links in `lat.md/` files can reference functions, constants, types,
  and methods. Use the repo-relative path: `[[config.go#parseConfig]]`, `[[version.go#version]]`,
  `[[server.go#agent#handleWrite]]` (Go method). `lat check` validates these exist.
- **Code refs**: `// @lat: [[section-id]]` (Go) or `# @lat: [[section-id]]` (shell) — ties source
  code to concepts.

# Test specs

Key tests are described as sections in `lat.md/tests.md`. Frontmatter can require that every leaf
section is referenced by a `// @lat:` comment in test code:

```markdown
---
lat:
  require-code-mention: true
---

# Tests

Wire-contract test specifications.

## Agent Core

Pins the daemon itself: CUPS parsing, discovery, health-gated failover, and the version surface.

### Queue Health Reads Status Text

Health is read from `lpstat` text, never the exit code — a disabled queue exits 0 and only an
absent one exits 1, so the exit code alone cannot decide it.

### Default Returns Empty Body Without A Printer

`GET /default` answers with a genuinely EMPTY body rather than an empty object when nothing is
healthy, because the transport reads empty text as "no default printer".
```

Every section MUST have a description — at least one sentence explaining what the test verifies
and why. Empty sections with just a heading are not acceptable.

Each test in code references its spec with exactly one comment placed next to the relevant test,
not at the top of the file:

```go
// @lat: [[tests#Agent Core#Queue Health Reads Status Text]]
func TestQueueHealthReadsStatusText(t *testing.T) {
	...
}
```

Do not duplicate refs. One `@lat:` comment per spec section, placed at the test that covers it.
`lat check` flags any spec section not covered by a code reference, and any code reference
pointing to a nonexistent section.

# Section structure

Every section in `lat.md/` **must** have a leading paragraph — at least one sentence immediately
after the heading, before any child headings or other block content. The first paragraph must be
≤250 characters (excluding `[[wiki link]]` content). It is the section's overview and is used in
search results, command output, and RAG context, so keeping it concise guarantees the section's
essence is always captured.

```markdown
# Good Section

Brief overview of what this section documents and why it matters.

More detail can go in subsequent paragraphs, code blocks, or lists.

## Child heading

Details about this child topic.
```

```markdown
# Bad Section

## Child heading

Details about this child topic.
```

The second example is invalid because `Bad Section` has no leading paragraph. `lat check`
validates this rule and reports errors for missing or overly long leading paragraphs.

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
