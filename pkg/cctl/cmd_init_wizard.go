package cctl

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The interactive `cctl init` wizard: a small three-step picker (tools →
// servers → restart) built on bubbletea. It only collects choices; the caller
// (runInit) does the actual work. Runs inline (no alt-screen) so the summary
// and per-tool output stay in the scrollback after it exits.

const (
	stepTools = iota
	stepServers
	stepRestart
	stepConfirm
)

// wizItem is one toggleable row in the tools or servers list.
type wizItem struct {
	key       string
	label     string
	on        bool
	localOnly bool // tools: applies to the local machine regardless of server picks
	local     bool // servers: this row is the local machine
}

// initSelection is what the wizard (or the flag parser) hands back to runInit.
type initSelection struct {
	tools         []string
	localSelected bool
	remotes       []string
	restart       bool
	canceled      bool
}

type initWizardModel struct {
	step     int
	tools    []wizItem
	servers  []wizItem
	restart  bool
	cursor   int
	canceled bool
	done     bool
}

// serverToolSelected reports whether any picked tool actually applies per
// server (tmux/mosh/claude). ghostty/cmux are local-only, so if only those are
// picked the server step is pointless and gets skipped.
func (m initWizardModel) serverToolSelected() bool {
	for _, t := range m.tools {
		if t.on && !t.localOnly {
			return true
		}
	}
	return false
}

// hasRemotes reports whether the servers list holds anything beyond local.
func (m initWizardModel) hasRemotes() bool {
	for _, s := range m.servers {
		if !s.local {
			return true
		}
	}
	return false
}

func (m initWizardModel) Init() tea.Cmd { return nil }

func (m initWizardModel) curLen() int {
	switch m.step {
	case stepTools:
		return len(m.tools)
	case stepServers:
		return len(m.servers)
	default:
		return 0
	}
}

func (m initWizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "esc", "q":
		m.canceled = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if n := m.curLen(); n > 0 && m.cursor < n-1 {
			m.cursor++
		}
	case " ", "x":
		m.toggle()
	case "enter":
		return m.advance()
	case "left", "h", "backspace":
		return m.back(), nil
	}
	return m, nil
}

func (m *initWizardModel) toggle() {
	switch m.step {
	case stepTools:
		m.tools[m.cursor].on = !m.tools[m.cursor].on
	case stepServers:
		m.servers[m.cursor].on = !m.servers[m.cursor].on
	case stepRestart:
		m.restart = !m.restart
	}
}

func (m initWizardModel) advance() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepTools:
		anyTool := false
		for _, t := range m.tools {
			anyTool = anyTool || t.on
		}
		if !anyTool {
			return m, nil // nothing picked — stay put
		}
		m.cursor = 0
		if m.serverToolSelected() && m.hasRemotes() {
			m.step = stepServers
		} else {
			m.step = stepRestart // no per-server choice to make; local is implied
		}
	case stepServers:
		anySrv := false
		for _, s := range m.servers {
			anySrv = anySrv || s.on
		}
		if !anySrv {
			return m, nil // nothing selected — stay (esc cancels); proceeding would silently imply local
		}
		m.cursor = 0
		m.step = stepRestart
	case stepRestart:
		m.step = stepConfirm
	case stepConfirm:
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m initWizardModel) back() initWizardModel {
	m.cursor = 0
	switch m.step {
	case stepServers:
		m.step = stepTools
	case stepRestart:
		if m.serverToolSelected() && m.hasRemotes() {
			m.step = stepServers
		} else {
			m.step = stepTools
		}
	case stepConfirm:
		m.step = stepRestart
	}
	return m
}

func (m initWizardModel) selection() initSelection {
	sel := initSelection{restart: m.restart, canceled: m.canceled}
	if m.canceled {
		return sel
	}
	for _, t := range m.tools {
		if t.on {
			sel.tools = append(sel.tools, t.key)
		}
	}
	for _, s := range m.servers {
		if !s.on {
			continue
		}
		if s.local {
			sel.localSelected = true
		} else {
			sel.remotes = append(sel.remotes, s.key)
		}
	}
	// If the server step was skipped (only local-only tools, or no remotes),
	// nothing turned local on explicitly — but the tools still target this
	// machine, so imply local.
	if len(sel.tools) > 0 && !sel.localSelected && len(sel.remotes) == 0 {
		sel.localSelected = true
	}
	return sel
}

func checkbox(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}

func (m initWizardModel) View() string {
	var b strings.Builder
	b.WriteString("cctl init\n\n")
	switch m.step {
	case stepTools:
		b.WriteString("Which tools to configure?  (space to toggle, enter to continue, esc to cancel)\n\n")
		for i, t := range m.tools {
			cur := "  "
			if i == m.cursor {
				cur = "> "
			}
			note := ""
			if t.localOnly {
				note = "  (local only)"
			}
			fmt.Fprintf(&b, "%s%s %s%s\n", cur, checkbox(t.on), t.label, note)
		}
	case stepServers:
		b.WriteString("Apply tmux/mosh/claude to which servers?  (space to toggle, enter to continue)\n\n")
		for i, s := range m.servers {
			cur := "  "
			if i == m.cursor {
				cur = "> "
			}
			fmt.Fprintf(&b, "%s%s %s\n", cur, checkbox(s.on), s.label)
		}
	case stepRestart:
		b.WriteString("After applying, restart all tracked sessions?\n")
		b.WriteString("(reloads tmux config + relaunches agents with claude --continue / codex resume)\n\n")
		fmt.Fprintf(&b, "  %s restart all tracked sessions\n\n", checkbox(m.restart))
		b.WriteString("space to toggle, enter to continue\n")
	case stepConfirm:
		b.WriteString("Ready:\n\n")
		sel := m.selection()
		fmt.Fprintf(&b, "  tools:   %s\n", strings.Join(sel.tools, ", "))
		targets := []string{}
		if sel.localSelected {
			targets = append(targets, "local")
		}
		targets = append(targets, sel.remotes...)
		fmt.Fprintf(&b, "  servers: %s\n", strings.Join(targets, ", "))
		fmt.Fprintf(&b, "  restart: %v\n\n", sel.restart)
		b.WriteString("enter to apply, esc to cancel, ← to go back\n")
	}
	return b.String()
}

// runInitWizard shows the interactive picker and returns the user's choices.
func runInitWizard(cfg *Config) (initSelection, error) {
	m := initWizardModel{
		tools: []wizItem{
			{key: "tmux", label: "tmux", on: true},
			{key: "ghostty", label: "ghostty", on: true, localOnly: true},
			{key: "mosh", label: "mosh", on: true},
			{key: "claude", label: "claude", on: true},
			{key: "cmux", label: "cmux", on: true, localOnly: true},
		},
	}
	m.servers = append(m.servers, wizItem{key: "local", label: "local (this machine)", on: true, local: true})
	for _, name := range sortedRemoteNames(cfg) {
		m.servers = append(m.servers, wizItem{key: name, label: name, on: true})
	}

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return initSelection{}, err
	}
	fm, ok := final.(initWizardModel)
	if !ok {
		return initSelection{}, fmt.Errorf("init wizard: unexpected model type")
	}
	return fm.selection(), nil
}
