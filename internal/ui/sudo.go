package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// sudoState tracks whether cached sudo credentials are available. Starting or
// stopping a tunnel consumes root privileges (sshuttle's pf firewall, and the
// split-DNS resolver files). The browser TUI owns the terminal, so sudo cannot
// prompt while it runs; instead the UI verifies the cached timestamp, offers
// authentication with the terminal handed back to sudo, and silently keeps the
// timestamp warm for the rest of the session.
type sudoState int

const (
	sudoUnknown sudoState = iota
	sudoReady             // cached timestamp verified
	sudoMissing           // no cached timestamp: authentication needed
)

const (
	// sudoWarmInterval is shorter than sudo's default 5-minute timestamp
	// timeout, so the noninteractive refresh always lands while the
	// timestamp is still valid.
	sudoWarmInterval = 4 * time.Minute

	// sudoAuthConfirm marks the modal that offers interactive sudo
	// authentication; Enter hands the terminal to sudo, Esc skips.
	sudoAuthConfirm = "sudo-auth"
)

// sudoRequest is the action a user wanted to run before sudo interrupted it.
type sudoRequest struct {
	action     string
	connection string
	view       string
}

type sudoCheckedMsg struct{ err error }
type sudoWarmTickMsg struct{}
type sudoWarmedMsg struct{ err error }
type sudoAuthDoneMsg struct{ err error }

// needsSudo reports whether an action consumes root privileges.
func needsSudo(action string) bool {
	return action == ActStart || action == ActStop
}

// checkSudoCmd verifies the cached sudo timestamp noninteractively; when then
// is non-nil the pending action resumes automatically after a successful check.
func (m appModel) checkSudoCmd() tea.Cmd {
	check := m.options.SudoCheck
	return func() tea.Msg { return sudoCheckedMsg{err: check()} }
}

// warmSudoCmd schedules the next silent keep-warm refresh.
func (m appModel) warmSudoCmd() tea.Cmd {
	return tea.Tick(sudoWarmInterval, func(time.Time) tea.Msg { return sudoWarmTickMsg{} })
}

// authenticateSudoCmd runs `sudo -v` with the real terminal: bubbletea
// suspends the TUI, sudo prompts for the password, and the TUI redraws
// cleanly afterwards.
func (m appModel) authenticateSudoCmd() tea.Cmd {
	authenticate := m.options.SudoAuthenticate
	return tea.ExecProcess(authenticate(), func(err error) tea.Msg { return sudoAuthDoneMsg{err: err} })
}

// guardSudo intercepts an action that needs root when the cached timestamp is
// not (yet) verified, and parks the request behind an authentication prompt.
// It reports whether the action was parked.
func (m *appModel) guardSudo(action, connection, view string) bool {
	if m.options.SudoCheck == nil || m.sudo == sudoReady || !needsSudo(action) {
		return false
	}
	m.sudoPending = &sudoRequest{action: action, connection: connection, view: view}
	m.actionID++
	m.actionModal = &actionModal{
		id: m.actionID, title: "Sudo authentication",
		body:          "Starting or stopping a tunnel needs sudo (firewall rules, split-DNS resolver files). Press Enter to authenticate — sudo will ask for your password — or Esc to skip.",
		confirmAction: sudoAuthConfirm, confirmConnection: connection,
	}
	return true
}

// resumeSudoPending re-issues the parked action once sudo is confirmed ready.
func (m appModel) resumeSudoPending() (tea.Model, tea.Cmd) {
	pending := *m.sudoPending
	m.sudoPending = nil
	m.actionModal = nil
	updated, actionCmd := m.requestAction(pending.action, pending.connection, pending.view)
	model := updated.(appModel)
	return model, tea.Batch(actionCmd, model.warmSudoCmd())
}
