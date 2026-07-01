# cctl

A TUI for managing [Claude Code](https://claude.com/claude-code) sessions
across local and remote (mosh + ssh + tmux) git checkouts. Each session
lives in its own [git worktree](https://git-scm.com/docs/git-worktree) on
its own branch, so you can run multiple long-running agents on the same
repo without stomping on each other.

Built around [cmux](https://github.com/manaflow-ai/cmux) for the
multi-workspace UI: each worktree gets one cmux workspace and every
session on it opens as a tab inside that workspace (`t` adds a plain
terminal tab, no claude); re-attaching focuses the existing workspace
instead of duplicating it.

## cmux compatibility

cctl drives cmux through its CLI (`new-workspace`, `list-workspaces`,
`workspace-group`, `rename-tab`, `notify`, `ssh`). Those interfaces move,
so we pin a known-good baseline here and review cmux's changelog against
it before bumping:

| | Version |
| --- | --- |
| **Tested with** | cmux **0.64.16** |
| **Changelog reviewed through** | cmux **v0.64.17** |
| **Minimum** | 0.64.x with `workspace-group` support (older releases untested) |

Check your local cmux with `cmux --version`. When bumping the tested
version, diff cmux's
[releases](https://github.com/manaflow-ai/cmux/releases) since the row
above and update it in the same change.

## Install

The recommended path is via [mise](https://mise.jdx.dev), which manages
both the Go toolchain and the resulting `cctl` shim in one command:

```bash
# Pin globally so cctl is always on $PATH:
mise use -g go:github.com/geoah/go-cctl/cmd/cctl@latest

# Or scope it to the current project's mise.toml:
mise use go:github.com/geoah/go-cctl/cmd/cctl@latest
```

mise will install a Go toolchain on demand, build the binary via
`go install`, and put it behind a shim called `cctl` on your `$PATH`.
Replace `@latest` with a tag (e.g. `@v0.1.0`) or commit SHA to pin.

If you'd rather use a system Go directly:

```bash
go install github.com/geoah/go-cctl/cmd/cctl@latest
```

Either way the binary is named `cctl`.

## Setup

cctl is config-driven — it needs to know which hosts to look at and
where to find git repos on them. Generate a starter config and edit it:

```bash
cctl init                       # writes ~/.cctl.yaml if absent
$EDITOR ~/.cctl.yaml
```

The minimum useful config has at least one server with one
`repo_sources` entry. A realistic example covering both a local box
and a remote workspace:

```yaml
defaults:
  branch_prefix: yourname        # branches go yourname/<name>
  worktree_base: ~/worktrees     # cctl puts every worktree here
  mosh: true                     # use mosh for remotes when available
  claude_flags: ["--dangerously-skip-permissions"]
  # agent: claude               # which CLI the TUI drives (claude|codex)
  # codex_flags: []             # flags passed to `codex` when agent: codex
  worktree_post_create:          # run inside each fresh worktree
    - mise trust
  # claude_update: claude update # what UU runs before restarting sessions
  # codex_update: codex update   # UU command when the resolved agent is codex

servers:
  local:
    local: true
    repo_sources:
      - path: ~/src/github.com   # walks recursively up to max_depth
        max_depth: 3
        default_branch: main

  workspace:
    host: 10.0.0.1
    user: you
    ssh_key: ~/.ssh/id_ed25519
    ssh_opts:
      - "-o"
      - "IdentitiesOnly=yes"
    # transport: cmux-ssh  # open sessions as cmux remote-SSH workspaces:
    #                      # Files panel lists the REMOTE fs, browser panes
    #                      # route through the remote, cmux CLI works there.
    #                      # Default (omitted) uses mosh/ssh in a local tab.
    repo_sources:
      - path: "~/"
        max_depth: 4
        default_branch: main
```

The knobs:

- **`servers.<name>`** — one block per host. `local: true` for the
  current machine; otherwise set `host:` + `user:` (+ optional
  `ssh_key:` / `ssh_opts:` for non-default ssh).
- **`repo_sources:`** — directories cctl walks (up to `max_depth`) to
  auto-discover git repos. Anything with a `.git` becomes a row in
  the tree without you having to enumerate it.
- **`repos:`** (optional, not shown) — explicit overrides for repos
  that live outside any `repo_sources` root or need a non-default
  branch.
- **`defaults.worktree_base`** — root for new worktrees
  (`<base>/<repo>/<worktree>`). The default is `~/worktrees`; cctl
  refuses to delete anything outside this path, so put it somewhere
  you're happy treating as scratch.
- **`defaults.worktree_post_create`** — list of shell commands run
  inside each freshly-created worktree (e.g. `mise trust`,
  `direnv allow`, `pre-commit install`). Best-effort; failures log
  but don't abort the session.
- **`defaults.branch_prefix`** — branches for new worktrees are named
  `<prefix>/<worktree-name>`.
- **`defaults.mosh: true`** uses [mosh](https://mosh.org) instead of
  ssh for remote sessions (better roaming + latency). Falls back
  to ssh per-server with `mosh: false`.
- **`agent`** — which CLI the TUI's session shortcuts launch: `claude`
  (default) or `codex`. Resolved most-specific-wins (repo → server →
  `defaults`), so you can run codex on one repo and claude everywhere
  else. `claude_flags` / `codex_flags` are passed through to the
  respective CLI; `claude_update` / `codex_update` are what the `UU`
  key runs before restarting sessions.

Re-run `cctl init` later for a refreshed example; it never overwrites
an existing `~/.cctl.yaml`. Run `cctl doctor` if a host or repo looks
wrong — it prints a per-server health report (transport, tools, repo
status, orphan sessions).

Then launch the TUI:

```bash
cctl
```

## Using the TUI

The TUI is a tree of servers → repos → worktrees → sessions. Keys:

| Key | Action |
| --- | --- |
| `↑`/`↓`, `j`/`k` | move the cursor |
| `←`/`→`, `h`/`l` | fold / unfold a server or repo |
| `PgUp`/`PgDn`, `⌃f`/`⌃b`, `⌃d`/`⌃u` | scroll by page / half-page |
| `g`/`G`, `Home`/`End` | jump to top / bottom |
| `⏎` | open or attach the selected row in a cmux tab |
| `n` | new session — opens the form (worktree, name, **agent**, prompt) |
| `t` | open a plain terminal tab on the worktree (no agent) |
| `dd` | delete (press `d` twice): on a session keeps the worktree; on a worktree removes it |
| `S` / `R` | reconcile — converge cmux + tmux to the manifest (revive, open, group, prune) |
| `UU` | upgrade the agent on the server (`claude_update`/`codex_update`) and restart its sessions |
| `/` | filter the tree; `r` refresh; `?` help; `q` quit |

In the **new-session form** (`n`), `Tab`/`↑↓` move between fields,
`←`/`→`/`Space` on the agent slot toggle **claude ↔ codex**, `⏎`
creates the session, and `Esc` cancels. The selected agent decides
which CLI launches; it defaults to the resolved `agent` for that
server/repo.

The same shortcuts can be scripted directly — see the CLI subcommands
below (`cctl claude`, `cctl codex`, `cctl ls`, `cctl rm`, …).

### Applying tmux/mosh config changes

tmux only reads `~/.tmux.conf` when its server starts, so edits from
`cctl init tmux` (locally or `--server <name>`) don't affect
already-running sessions. To roll them out everywhere:

```bash
cctl --restart-all        # reload tmux config on every server, then
                          # destroy + restore all tracked sessions
```

This reloads each server's tmux config, kills every tracked session,
and lets the startup reconcile revive them — `claude --continue` /
`codex resume --last` resume the conversations, and the fresh
attach/mosh reconnect picks up the new clipboard/scroll settings. It
prompts first (bypass with `-y`), and only touches cctl's own
sessions, not unrelated tmux sessions on the host.

## What it does

- Discovers git repos under configured roots (local + remote).
- Lets you start a new Claude session on a repo (worktree auto-created)
  or attach to an existing one. Single keystroke for both.
- `cctl codex` is the OpenAI Codex equivalent of `cctl claude`: same
  target/session/worktree semantics, but it launches the `codex` CLI
  (configure flags via `codex_flags`). Codex has no
  `--session-id`, so resume is keyed on the worktree directory
  (`codex resume --last`).
- The TUI's session shortcuts (new session, attach/respawn, the `U`
  upgrade, reconcile revival) follow the resolved **`agent`** for each
  server/repo — set `agent: codex` at `defaults`, on a server, or on a
  repo and every shortcut drives codex there instead of claude
  (`codex_update` overrides the `U`-key command, default `codex update`).
- Names tmux sessions `cctl/<repo>/<worktree>/<session>` and matches them
  back into the tree, so the TUI is the single source of truth.
- Guards aggressively against deleting anything outside `worktree_base` —
  the repo itself can never be removed via `dd`.
- Per-server connection status (probe + retry/backoff) with spinners on
  rows that are still loading.

## Layout

- `cmd/cctl/main.go` — entry point (so `go install …/cmd/cctl@latest`
  produces a binary called `cctl`), thin shim around `pkg/cctl.Run()`.
- `pkg/cctl/` — all logic (TUI, CLI, spawner, config, discovery, scripts).
- `AGENTS.md` — guidance for AI agents and contributors editing the
  package (especially the testing rules).
- `.mise.toml` — task definitions for `mise run cctl:install` etc.

## License

MIT.
