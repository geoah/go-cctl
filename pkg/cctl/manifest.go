package cctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The restore manifest is cctl's durable memory of "which session tabs
// should exist". cctl otherwise derives everything from the live tmux
// server — but a machine reboot wipes tmux, leaving cctl amnesiac: it can't
// restore sessions it no longer knows about. The manifest closes that gap.
// It lives at ~/.cctl/workspaces.json, is updated on every spawn and every
// delete, and is the source of truth the sync/reconcile pass restores from.
//
// Identity key is (server, repo, worktree, session) — the same tuple that
// names the tmux session and the durable wrapper script.

// wsEntry is one tracked session tab.
type wsEntry struct {
	Server   string `json:"server"`
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
	Session  string `json:"session"`
	// TmuxName is the canonical (sanitized) tmux session name, cached so
	// reconcile can match against `tmux list-sessions` without re-deriving.
	TmuxName string `json:"tmux_name"`
	WsTitle  string `json:"ws_title"`  // cmux workspace name: "repo/worktree/session"
	TabTitle string `json:"tab_title"` // cmux tab title: the session name
	Group    string `json:"group"`     // sidebar group
	GroupCwd string `json:"group_cwd"`
	Cwd      string `json:"cwd"`    // workspace cwd hint
	Script   string `json:"script"` // durable wrapper path
	Remote   bool   `json:"remote"` // cmux-ssh transport (no local resume binding)
	// Agent is the coding agent this session was created with ("claude" or
	// "codex"). Empty for sessions created before per-session agent tracking
	// (and for adopted sessions) — callers fall back to the config-resolved
	// agent, so old manifests keep working.
	Agent   string `json:"agent,omitempty"`
	Updated int64  `json:"updated"`
}

func (e wsEntry) key() string {
	return manifestKey(e.Server, e.Repo, e.Worktree, e.Session)
}

func manifestKey(server, repo, worktree, session string) string {
	return strings.Join([]string{server, repo, worktree, session}, "\x00")
}

// manifestMu serializes the load-modify-save cycle. Spawns and deletes run
// in bubbletea command goroutines, so concurrent writers are possible; the
// on-disk file is the source of truth and the lock keeps writes atomic
// relative to each other.
var manifestMu sync.Mutex

func manifestPath() (string, error) {
	h, err := cctlHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "workspaces.json"), nil
}

// manifestFileExists reports whether the manifest has ever been written.
// The sync pass uses this to bootstrap safely on first run: with no
// established manifest it adopts existing tabs instead of treating them as
// orphans to close.
func manifestFileExists() bool {
	p, err := manifestPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// loadManifest reads the manifest (an empty slice when absent). Callers that
// mutate must hold manifestMu around load+save.
func loadManifest() []wsEntry {
	p, err := manifestPath()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var doc struct {
		Entries []wsEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		log().Warn("manifest-parse-fail", "path", p, "err", err.Error())
		return nil
	}
	return doc.Entries
}

// loadManifestEntries is the read-locked accessor for callers that only read.
func loadManifestEntries() []wsEntry {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	return loadManifest()
}

// saveManifest writes the manifest atomically (temp + rename). Entries are
// sorted by key for a stable, diff-friendly file.
func saveManifest(entries []wsEntry) error {
	p, err := manifestPath()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
	doc := struct {
		Entries []wsEntry `json:"entries"`
	}{Entries: entries}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// manifestAgent returns the recorded agent for a session, or "" if the session
// isn't tracked or predates per-session agent tracking. Used so attaching to an
// existing session re-records its real agent instead of the config default.
func manifestAgent(server, repo, worktree, session string) string {
	want := wsEntry{Server: server, Repo: repo, Worktree: worktree, Session: session}.key()
	manifestMu.Lock()
	defer manifestMu.Unlock()
	for _, e := range loadManifest() {
		if e.key() == want {
			return e.Agent
		}
	}
	return ""
}

// manifestUpsert records (or refreshes) a session from a successful spawn.
func manifestUpsert(spec SpawnSpec) {
	if !spec.hasIdentity() {
		return
	}
	e := wsEntry{
		Server:   spec.Server,
		Repo:     spec.Repo,
		Worktree: spec.Worktree,
		Session:  spec.Session,
		TmuxName: tmuxName(spec.Repo, spec.Worktree, spec.Session),
		WsTitle:  spec.WsTitle,
		TabTitle: spec.TabTitle,
		Group:    spec.GroupTitle,
		GroupCwd: spec.GroupCwd,
		Cwd:      spec.Cwd,
		Script:   spec.Script,
		Remote:   spec.Remote != nil,
		Agent:    spec.Agent,
		Updated:  time.Now().Unix(),
	}
	manifestMu.Lock()
	defer manifestMu.Unlock()
	entries := loadManifest()
	replaced := false
	for i := range entries {
		if entries[i].key() == e.key() {
			entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, e)
	}
	if err := saveManifest(entries); err != nil {
		log().Warn("manifest-save-fail", "err", err.Error())
	}
}

// manifestUpsertEntry records a fully-formed entry (used by the sync pass to
// adopt sessions discovered in tmux/cmux that predate the manifest).
func manifestUpsertEntry(e wsEntry) {
	if e.Server == "" || e.Repo == "" || e.Worktree == "" || e.Session == "" {
		return
	}
	if e.Updated == 0 {
		e.Updated = time.Now().Unix()
	}
	manifestMu.Lock()
	defer manifestMu.Unlock()
	entries := loadManifest()
	for i := range entries {
		if entries[i].key() == e.key() {
			entries[i] = e
			if err := saveManifest(entries); err != nil {
				log().Warn("manifest-save-fail", "err", err.Error())
			}
			return
		}
	}
	entries = append(entries, e)
	if err := saveManifest(entries); err != nil {
		log().Warn("manifest-save-fail", "err", err.Error())
	}
}

// manifestDedupByTmux collapses entries that describe the SAME session — same
// (server, canonical tmux name) — into one, healing the historical skew where
// a spawn recorded a dotted worktree by its real name ("ecr.b1") while adopt
// recorded the same live session with the tmux-sanitized name ("ecr_b1"). Two
// rows for one tmux session mean a duplicate cmux workspace the reconcile keeps
// re-opening and a dd that can't fully remove the session. Keeps the entry
// whose repo/worktree are the REAL names (they change under sanitization),
// else the most recently updated. Wrapper scripts are left alone — the twins
// share one session/script, so the survivor still needs it. Returns how many
// entries it dropped.
func manifestDedupByTmux() int {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	entries := loadManifest()
	// realness scores how "un-sanitized" an entry's identity is: a name that
	// changes when re-sanitized is the original real one (the twin we keep).
	realness := func(e wsEntry) int {
		score := 0
		if tmuxSafeName(e.Repo) != e.Repo {
			score++
		}
		if tmuxSafeName(e.Worktree) != e.Worktree {
			score++
		}
		return score
	}
	// wins reports whether a should be kept over b: prefer the realer name,
	// then the more recently updated entry.
	wins := func(a, b wsEntry) bool {
		if ra, rb := realness(a), realness(b); ra != rb {
			return ra > rb
		}
		return a.Updated > b.Updated
	}
	best := map[string]int{} // server\x00tmuxName -> index of the kept entry
	drop := map[int]bool{}
	for i, e := range entries {
		if e.TmuxName == "" {
			continue // no canonical identity to dedup on
		}
		k := e.Server + "\x00" + e.TmuxName
		j, seen := best[k]
		if !seen {
			best[k] = i
			continue
		}
		if wins(e, entries[j]) {
			best[k] = i
			drop[j] = true
		} else {
			drop[i] = true
		}
	}
	if len(drop) == 0 {
		return 0
	}
	kept := entries[:0]
	for i, e := range entries {
		if !drop[i] {
			kept = append(kept, e)
		}
	}
	if err := saveManifest(kept); err != nil {
		log().Warn("manifest-save-fail", "err", err.Error())
	}
	return len(drop)
}

// manifestRemoveWorktree drops every session on a worktree (called when the
// whole worktree is removed) and best-effort deletes their wrapper scripts.
func manifestRemoveWorktree(server, repo, worktree string) {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	entries := loadManifest()
	kept := entries[:0]
	var dropped []wsEntry
	for _, e := range entries {
		if e.Server == server && e.Repo == repo && e.Worktree == worktree {
			dropped = append(dropped, e)
			continue
		}
		kept = append(kept, e)
	}
	if len(dropped) == 0 {
		return
	}
	if err := saveManifest(kept); err != nil {
		log().Warn("manifest-save-fail", "err", err.Error())
	}
	for _, e := range dropped {
		if e.Script != "" {
			if err := os.Remove(e.Script); err != nil && !os.IsNotExist(err) {
				log().Debug("manifest-script-remove-fail", "script", e.Script, "err", err.Error())
			}
		}
	}
}

// manifestRemove drops a session (called when it's deleted) and best-effort
// removes its durable wrapper script. It matches on the (server,repo,worktree,
// session) key AND on the canonical tmux name: tmuxName sanitizes its inputs,
// so it's the same for a real-worktree entry and a legacy sanitized-worktree
// twin (e.g. "ecr.b1" and "ecr_b1" both → cctl/<repo>/ecr_b1/<session>).
// Matching either purges every record of the session — otherwise a surviving
// twin gets revived by the next reconcile and the session looks un-deletable.
func manifestRemove(server, repo, worktree, session string) {
	key := manifestKey(server, repo, worktree, session)
	tname := tmuxName(repo, worktree, session)
	manifestMu.Lock()
	defer manifestMu.Unlock()
	entries := loadManifest()
	kept := entries[:0]
	var removed []wsEntry
	for _, e := range entries {
		if e.key() == key || (e.Server == server && e.TmuxName == tname) {
			removed = append(removed, e)
			continue
		}
		kept = append(kept, e)
	}
	if len(removed) == 0 {
		return
	}
	if err := saveManifest(kept); err != nil {
		log().Warn("manifest-save-fail", "err", err.Error())
	}
	for _, e := range removed {
		if e.Script != "" {
			if err := os.Remove(e.Script); err != nil && !os.IsNotExist(err) {
				log().Debug("manifest-script-remove-fail", "script", e.Script, "err", err.Error())
			}
		}
	}
}
