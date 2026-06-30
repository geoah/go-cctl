# AI Agent Guide — go-cctl

This is the home of `cctl`. It manages mosh + tmux + claude sessions,
with a Bubble Tea TUI and a cobra CLI. All logic lives in
[`pkg/cctl`](pkg/cctl/); [`cmd/cctl/main.go`](cmd/cctl/main.go) is a
thin shim that calls `cctl.Run()`. The binary lives under `cmd/` (not
the repo root) so `go install …/cmd/cctl@latest` produces a binary
named `cctl` rather than `go-cctl`.

## ⚠️ TESTING IS NON-NEGOTIABLE ⚠️

A lot of the bugs we've shipped here came from "small" untested changes —
the parser-and-script layer is shell-out-driven and ships breakage fast.
Don't skip tests. The rules:

### 1. Always run the tests before reporting a change as done

```bash
mise run cctl:test                    # unit tests (always)
mise run cctl:test:integration        # local + workspace round-trips
```

Both suites must be **green** before you commit. Not "compiles fine" —
green. This is in addition to (not instead of) `mise run precommit`.

The integration suite (`-tags=integration`) drives real ssh, real git,
and real tmux on local and (if reachable) the `workspace` server in
`~/.cctl.yaml`. Workspace tests skip themselves automatically when the
host is unreachable — never silence a failure by deleting a test;
fix the underlying bug.

If your change touches anything in the lifecycle of a session (create
worktree → start tmux → list → kill → remove worktree), exercise both
servers via the integration suite. The unit tests catch script-builder
regressions; the integration tests catch quoting/ssh/tmux/git
interaction regressions.

### 2. Any new behaviour ships with a new test

If you add a new function, change an existing parser, alter a shell script
builder, or wire a new keystroke / config option: **write a test for it in
the same change**. Don't promise to add tests later.
[`pkg/cctl/parse_test.go`](pkg/cctl/parse_test.go) is the model:

- Pure functions (parsers, name-assigners, script builders) get direct
  unit tests with concrete inputs and `t.Errorf`/`t.Fatalf` assertions.
- Env-driven helpers (e.g. `detectSpawner`) use `t.Setenv` to control
  every signal the function reads, and assert on the chosen branch
  _and_ the reason string.
- Anything that emits a shell script is tested for both **presence**
  of expected lines and **structural ordering** (so we catch
  "post-create runs before the worktree exists" bugs).

If your change can't be unit-tested cleanly, that's a smell: refactor
the logic into a pure function that can be, then write the test.

### 3. Tests must actually exercise the behaviour they claim to

`func TestFoo(t *testing.T) { Foo(...) }` with no assertions is worse
than no test — it gives false confidence. Every test must:

- Construct realistic input (snippets of `tmux ls` output, `git worktree
  list --porcelain` blocks, env-var combinations, etc.).
- Assert on the **observable** behaviour: returned values, fields, the
  presence and ordering of specific lines in generated scripts.
- Fail loudly with a useful message that includes both expected and
  actual values.

If you're tempted to write a "tautological" test that just verifies the
code ran, stop and ask whether the function under test even needs to
exist.

### 4. When a bug is reported, write the test first

The fastest way to fix a bug that's bitten us once is to encode the
reproducer as a failing test, then make it pass. Examples in this
package — every one of these should have shipped with the originating
change instead of being fixed later:

- A `samePath` regression that mislabelled the main checkout as a
  normal worktree and let `dd` `rm -rf` the user's repo (now
  `TestSamePath_TildeMatchesExpandedHome` + the three-layer guard).
- `pickRepoSource` picked `$HOME` over `~/src/github.com` clusters
  (now `TestPickRepoSource_PrefersDeeperMostPopulated`).
- `ensureWorktreeScript` skipped post-create hooks on existing
  worktrees (now `TestEnsureWorktreeScript_RunsPostCreateOnExisting`).

If a regression is "obvious in hindsight," the test is too.

## Layout cheatsheet

- **`pkg/cctl/spawn.go`** — terminal-emulator dispatch (cmux only).
  `detectSpawner` is the entry point; new providers plug in via
  `allSpawners()`.
- **`pkg/cctl/agent.go`, `cmd_claude.go`, `cmd_codex.go`** — the
  claude/codex agent abstraction. `config.go`'s `resolve()` picks the
  agent for a target (repo → server → `defaults`, via `firstNonEmpty`);
  `agent.go` turns that into launch/attach/update scripts; the two
  `cmd_*` files are the CLI entry points. New agents plug in here, not
  in the TUI.
- **`pkg/cctl/session.go`, `worktree_list.go`, `discover.go`,
  `worktree.go`** — pure-ish data layer (parsers, name assignment,
  shell-script builders). This is where most tests live and should live.
- **`pkg/cctl/tui.go`** — Bubble Tea model. Don't drop logic here that
  should be pure; extract a helper and test it.
- **`pkg/cctl/config.go`** — YAML schema + resolve. Keep new keys
  retrofittable (struct tags should always make absent values benign).
- **`pkg/cctl/parse_test.go`** — current unit test suite. Grow it;
  don't replace it.
- **`pkg/cctl/integration_test.go`** (`//go:build integration`) — real-
  environment round trips on local + workspace. Always extend this when
  you add or fix anything in the session lifecycle.

## ⚠️ Build + install loop — ALWAYS install after a change ⚠️

**Every change touching cctl ends with `mise run cctl:install`. No exceptions.**
The user runs cctl from `$PATH` (`~/.local/bin/cctl`); a change that
compiles but isn't installed is invisible to them and gives false
"it's fixed" signals. Installing is part of "done", same as testing.

After any change touching cctl, run the full loop:

```bash
go build ./...
mise run cctl:test
mise run cctl:test:integration   # require this when changing session lifecycle
mise run cctl:install            # MANDATORY — every change, not just "final" ones
```

`cctl:install` writes the binary to `~/.local/bin/` so it survives Go
version upgrades and stays on `$PATH` even when mise's go shim isn't
active. If you made several edits across a turn, install at the end of
the turn so the running binary always reflects the latest code.

## Versioning, changelog & releases (release-please, in CI)

[Semantic versioning](https://semver.org/) + [Keep a Changelog](https://keepachangelog.com/)
from [Conventional Commit](https://www.conventionalcommits.org/) subjects, driven
by [release-please](https://github.com/googleapis/release-please) in GitHub
Actions. You never edit the version or changelog by hand, and you never tag
manually.

The flow:

```
merge Conventional Commits to main
  → release-please opens/updates a "chore(main): release X.Y.Z" PR
    (feat → minor, fix/perf → patch, <type>! or "BREAKING CHANGE" → major;
     pre-1.0 a breaking change bumps minor — see bump-minor-pre-major)
  → the PR bumps version.txt and regenerates the CHANGELOG.md section
  → merging that PR tags vX.Y.Z and publishes the GitHub Release
```

Config lives in `release-please-config.json` (release-type `simple`, which
tracks the version in `version.txt`) + `.release-please-manifest.json` (the
last released version). The workflow is `.github/workflows/release-please.yml`;
it needs the one-time repo setting **Settings → Actions → General → "Allow
GitHub Actions to create and approve pull requests."** Other commit types
(`docs`/`chore`/`refactor`/`test`/`ci`/`build`/`style`) don't trigger a release
on their own — release-please just rolls them into the next feat/fix release.

`version.txt` is embedded into the binary at build time via the mise tasks and
shown by `cctl version`; a bare `go install …@vX` recovers the version from the
module build info instead.

CI runs on every push + PR (`.github/workflows/ci.yml`: gofmt, `go vet`,
`go build`, `go test -race`). The unit suite runs there; the integration suite
(`//go:build integration`) needs real ssh/tmux/workspace and only runs locally
(`mise run cctl:test:integration`) — run it yourself for lifecycle changes.
PR titles are linted as Conventional Commits (`.github/workflows/pr-title.yml`)
because we squash-merge, so the title becomes the commit release-please parses;
the local `commit-msg` hook mirrors this for your own commits (enable once with
`mise run hooks:setup`; bypass a one-off with `CCTL_NO_LINT=1 git commit …`).
