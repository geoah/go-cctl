# cctl

A TUI for managing [Claude Code](https://claude.com/claude-code) sessions
across local and remote (mosh + ssh + tmux) git checkouts. Each session
lives in its own [git worktree](https://git-scm.com/docs/git-worktree) on
its own branch, so you can run multiple long-running agents on the same
repo without stomping on each other.

Built around [cmux](https://github.com/manaflow-ai/cmux) for the
multi-workspace UI: every session opens as a new cmux tab; re-attaching
focuses the existing tab instead of duplicating it.

## Install

```bash
# via mise (manages Go too)
mise use go@latest
go install github.com/geoah/go-cctl@latest

# or with a system Go
go install github.com/geoah/go-cctl@latest
```

The binary is installed as `cctl`. Run `cctl init` to generate a starter
`~/.cctl.yaml`, then `cctl` to open the TUI.

## What it does

- Discovers git repos under configured roots (local + remote).
- Lets you start a new Claude session on a repo (worktree auto-created)
  or attach to an existing one. Single keystroke for both.
- Names tmux sessions `cctl/<repo>/<worktree>/<session>` and matches them
  back into the tree, so the TUI is the single source of truth.
- Guards aggressively against deleting anything outside `worktree_base` —
  the repo itself can never be removed via `dd`.
- Per-server connection status (probe + retry/backoff) with spinners on
  rows that are still loading.

## Layout

- `main.go` — entry point, calls `pkg/cctl.Run()`.
- `pkg/cctl/` — all logic (TUI, CLI, spawner, config, discovery, scripts).
- `AGENTS.md` — guidance for AI agents and contributors editing the
  package (especially the testing rules).
- `.mise.toml` — task definitions for `mise run cctl:install` etc.

## License

MIT.
