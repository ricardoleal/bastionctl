// app.go implements the two-level k9s-style bastionctl browser:
//
//	level 0 (home)        — all saved connections as a status table, with
//	                        single-key actions (start/stop/logs/…) on the
//	                        highlighted row
//
// level 1 (connection) — full state card for one connection plus its
//
//	action row
//
// RunApp always runs to an Outcome: either the user quit, or a single action
// was requested (from a hotkey stroke, or from the `:` command line). The
// caller executes the action on the real terminal — sudo prompts, log
// following, and foreground tunnels need the plain screen — and then re-enters
// RunApp with freshly gathered data at the requested view.
package ui

import (
	"fmt"
	"net/netip"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"bastionctl/internal/config"
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// Action ids the app can report in an Outcome. Shared by hotkeys and the
// `:` command line.
const (
	ActOpen        = "open"
	ActNew         = "new"
	ActStart       = "start"
	ActLaunch      = "launch"
	ActStop        = "stop"
	ActStatus      = "status"
	ActLogs        = "logs"
	ActPrint       = "print"
	ActRescan      = "rescan"
	ActForget      = "forget"
	ActDNSSetup    = "dns-setup"
	ActDNSTeardown = "dns-teardown"
	ActDNSEdit     = "dns-edit"
	ActEdit        = "edit"
	ActAWSCheck    = "aws-check"
)

// Views an Outcome can request for the next RunApp entry.
const (
	ViewHome       = "home"
	ViewConnection = "connection"
)

// ConnSummary flattens one saved connection's live status for the home table.
// main.go gathers it cheaply: pid-file stat, resolver state file read, and a
// bounded reachability probe per non-running connection.
type ConnSummary struct {
	Account     string // AWS account id owning the connection
	Name        string
	Kind        string
	Method      string
	Region      string
	AWSProfile  string
	Networks    string
	DNS         string // DNSConfig.Summary() or "off"
	Running     bool
	PID         int
	Reachable   bool
	Unreachable bool // stopped and the saved target is not reachable right now
	Stale       bool // resolver config missing though auto-management expected it
}

func (s ConnSummary) AWSProfileLabel() string {
	if s.AWSProfile == "" {
		return "default"
	}
	return s.AWSProfile
}

// StatusLabel renders a plain-text home-table status cell. The bubbles table
// truncates embedded ANSI escapes as visible text, so keep this cell unstyled.
func (s ConnSummary) StatusLabel() string {
	switch {
	case s.Running && s.Stale:
		return "◐ Running, resolver missing"
	case s.Running:
		return "● Running"
	case s.Unreachable:
		return "● Unreachable"
	case s.Reachable:
		return "● Reachable"
	default:
		return "○ Idle"
	}
}

// DotState is the bare state name ("ok"/"warn"/"fail"/"idle").
func (s ConnSummary) DotState() string {
	switch {
	case s.Running && s.Stale:
		return "warn"
	case s.Running:
		return "ok"
	case s.Unreachable:
		return "fail"
	case s.Reachable:
		return "ok"
	default:
		return "idle"
	}
}

// ConnDetail is the Level-1 card payload: everything the connection screen
// shows without new AWS calls (routes/target came from the saved config).
type ConnDetail struct {
	Name            string
	Kind            string
	Method          string
	Region          string
	AWSProfile      string
	SSHPort         int
	Running         bool
	PID             int
	Target          string
	TargetOK        bool
	ReachDetail     string
	DNSSummary      string // "" when off
	DNSInstalled    string // state.Describe(); "" when nothing was ever installed
	DNSWantsInstall bool   // split mode with nothing installed yet
	Domains         []string
	Resolver        []string
	Routes          []config.NetworkRoute
	Networks        []string // selected CIDRs, for the card footer line
	Command         string   // full sshuttle command preview
}

// Outcome is the result of one RunApp session.
type Outcome struct {
	Action     string // "" when nothing should be executed
	Connection string // target connection name ("" for global actions)
	Quit       bool   // user invoked q/quit: do not re-enter
	View       string // where to land on re-entry (ViewHome / ViewConnection)
}

// ActionResult is the content shown after an in-app action completes.
type ActionResult struct {
	Title            string
	Body             string
	Refresh          bool
	RemoveConnection bool
}

// ActionHandler runs an action without taking ownership of the terminal.
type ActionHandler func(connection string) (ActionResult, error)

type ConfigLoader func(string) (*config.Config, error)
type ConfigSaver func(string, *config.Config) error
type AWSProfileChecker func(*config.Config, string) (ActionResult, error)

type RescanCandidate struct {
	Name       string
	InstanceID string
	Method     string
	Target     string
	VpcID      string
	VpcCIDRs   []string
	SubnetID   string
	Reachable  bool
	EgressOK   bool
	Current    bool
}

type NewConnectionStart struct {
	Regions    []string
	AccountID  string
	AWSProfile string
	SSHPort    int
}

// LogSession is a cancellable stream owned by the interactive UI.
type LogSession struct {
	Lines  <-chan string
	Done   <-chan error
	Cancel func()
}

// ---------------------------------------------------------------------------
// RunApp entry point
// ---------------------------------------------------------------------------

// RunOptions parametrizes one RunApp session.
type RunOptions struct {
	Account           string                            // header context (account id/alias)
	Profile           string                            // AWS profile shown in the header ("" = default)
	Offline           bool                              // AWS is unreachable: browse cached connections only
	Error             string                            // error carried back into the next browser frame
	Level             int                               // 0 = home, 1 = connection view
	Highlight         int                               // home row to place the cursor on
	Summaries         []ConnSummary                     // home table content
	Detail            *ConnDetail                       // Level-1 card content (required at level 1)
	LoadDetail        func(string) (*ConnDetail, error) // loads a connection without leaving the UI
	Actions           map[string]ActionHandler          // actions that run inside the active UI
	StartLogs         func(string) (*LogSession, error) // starts logs without taking terminal ownership
	StartLaunch       func(string) (*LogSession, error) // starts foreground launch in a captured output view
	LoadConfig        ConfigLoader
	SaveConfig        ConfigSaver
	AWSProfiles       []string
	AWSRegions        []string
	CheckAWSProfile   AWSProfileChecker
	RefreshConnection func(string) (ConnSummary, *ConnDetail, error)
	ConfigSnapshot    []*config.Config
	RefreshConfigs    func() ([]*config.Config, error)
	RefreshStatus     func() []ConnSummary // re-probes running/reachable state for every connection
	PrepareRescan     func(string) ([]RescanCandidate, error)
	SaveRescan        func(string, RescanCandidate) (*config.Config, error)
	StartNew          func() (NewConnectionStart, error)
	ScanNew           func(string, string) ([]RescanCandidate, error)
	PrepareNewRoutes  func(string, string, RescanCandidate) ([]config.NetworkRoute, error)
	SaveNewConfig     func(*config.Config) error
	// SudoCheck verifies the cached sudo timestamp noninteractively
	// (`sudo -n -v`); nil disables all sudo handling (demo mode, tests).
	// SudoAuthenticate returns the interactive `sudo -v` command the UI
	// runs via tea.ExecProcess when the user accepts authentication.
	SudoCheck        func() error
	SudoAuthenticate func() *exec.Cmd
}

// RunApp opens the interactive two-level browser and returns its outcome.
func RunApp(options RunOptions) (Outcome, error) {
	model := appModel{
		options:      options,
		knownConfigs: options.ConfigSnapshot,
		home:         newHomeTable(options.Summaries, options.Highlight),
		errorMsg:     options.Error,
		outcome:      Outcome{View: ViewHome},
	}
	if options.Level == 1 {
		if options.Detail == nil {
			return Outcome{Quit: true}, fmt.Errorf("connection view requested without detail data")
		}
		model.outcome.Connection = options.Detail.Name
		model.outcome.View = ViewConnection
	}
	result, err := tea.NewProgram(model, tea.WithAltScreen()).Run()
	if err != nil {
		return Outcome{Quit: true}, err
	}
	return result.(appModel).outcome, nil
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type appModel struct {
	options        RunOptions
	knownConfigs   []*config.Config
	width          int
	height         int
	home           table.Model
	searching      bool
	searchInput    string
	searchFilter   string
	searchFilters  map[string]string
	searchTarget   string
	searchOriginal string
	shell          bool // the `:` command line is open
	shellInput     string
	shellIndex     int    // tab-completion cycle position
	help           bool   // the `?` navigation help overlay is shown
	errorMsg       string // cleared on the next key press
	loadingDetail  string
	actionID       int
	actionModal    *actionModal
	logSession     *LogSession
	logLines       []string
	logOffset      int
	logDone        bool
	logError       error
	logTitle       string
	editor         *connectionEditor
	rescan         *rescanSelector
	newWizard      *newConnectionWizard
	sudo           sudoState
	sudoPending    *sudoRequest
	outcome        Outcome
}

const (
	searchHome          = "home"
	searchDetail        = "connection"
	searchEditor        = "editor"
	searchEditorChoices = "editor-choices"
	searchEditorRoutes  = "editor-routes"
	searchRescan        = "rescan"
	searchLogs          = "logs"
	searchNewCandidates = "new-candidates"
	searchNewRoutes     = "new-routes"
	searchNewRegions    = "new-regions"
	searchNewKinds      = "new-kinds"
)

type detailLoadedMsg struct {
	name   string
	detail *ConnDetail
	err    error
}

type actionModal struct {
	id                int
	title             string
	body              string
	running           bool
	failed            bool
	confirmAction     string
	confirmConnection string
}

type actionFinishedMsg struct {
	id                int
	result            ActionResult
	err               error
	savedConfig       *config.Config
	originalName      string
	connection        string
	newConnection     bool
	reachabilityKnown bool
	targetReachable   bool
}

type modalDismissTickMsg struct{ id int }

// actionModalAutoDismiss is how long a completed action message box
// stays on screen before clearing itself. Failed actions keep waiting
// for explicit dismissal so the error is not lost.
const actionModalAutoDismiss = 4 * time.Second

func dismissModalAfter(id int) tea.Cmd {
	return tea.Tick(actionModalAutoDismiss, func(time.Time) tea.Msg {
		return modalDismissTickMsg{id: id}
	})
}

// scheduleModalDismiss arranges self-clearing for a completed action
// message box that is still on screen. It is called at every point
// where follow-up work (table refresh, detail load) has finished, so
// the timer only starts once the modal is in its final state.
func (m appModel) scheduleModalDismiss() tea.Cmd {
	if m.actionModal == nil || m.actionModal.running || m.actionModal.failed || m.actionModal.confirmAction != "" {
		return nil
	}
	return dismissModalAfter(m.actionModal.id)
}

type logLineMsg string
type logDoneMsg struct{ err error }
type configLoadedMsg struct {
	cfg        *config.Config
	err        error
	startField int
}

type configRefreshTickMsg struct{}
type statusTickMsg struct{}
type statusRefreshedMsg struct{ summaries []ConnSummary }
type configRefreshedMsg struct {
	configs []*config.Config
	err     error
}
type rescanCandidatesMsg struct {
	id         int
	connection string
	candidates []RescanCandidate
	err        error
}

type rescanSelector struct {
	connection string
	candidates []RescanCandidate
	cursor     int
}

type newConnectionWizard struct {
	step       string
	start      NewConnectionStart
	region     string
	kind       string
	candidates []RescanCandidate
	cursor     int
	candidate  RescanCandidate
	routes     []config.NetworkRoute
}

type newConnectionStartMsg struct {
	id   int
	data NewConnectionStart
	err  error
}

type newCandidatesMsg struct {
	id         int
	region     string
	kind       string
	candidates []RescanCandidate
	err        error
}

type newRoutesMsg struct {
	id     int
	routes []config.NetworkRoute
	err    error
}

type connectionEditor struct {
	cfg           *config.Config
	originalName  string
	field         int
	input         string
	fields        []string
	values        []string
	profiles      []string
	regions       []string
	conflicted    bool
	isNew         bool
	choices       []string
	choiceCursor  int
	routePicker   bool
	routeCursor   int
	routes        []config.NetworkRoute
	routeOriginal []config.NetworkRoute
	addingRoute   bool
	routeInput    string
	routeError    string
}

type connectionRefreshedMsg struct {
	id      int
	name    string
	summary ConnSummary
	detail  *ConnDetail
	err     error
}

func (m appModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.options.RefreshConfigs != nil {
		cmds = append(cmds, m.nextConfigRefresh())
	}
	if m.options.RefreshStatus != nil {
		cmds = append(cmds, m.nextStatusRefresh())
	}
	if m.options.SudoCheck != nil {
		cmds = append(cmds, m.checkSudoCmd())
	}
	return tea.Batch(cmds...)
}

func (m appModel) level() int {
	if m.options.Level == 1 && m.options.Detail != nil {
		return 1
	}
	return 0
}

// currentName is the connection the context targets: the detail connection on
// level 1, the highlighted home row otherwise.
func (m appModel) currentName() string {
	if m.level() == 1 {
		return m.options.Detail.Name
	}
	summaries := m.visibleSummaries()
	cursor := m.home.Cursor()
	if cursor < 0 || cursor >= len(summaries) {
		return ""
	}
	return summaries[cursor].Name
}

func (m appModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case configRefreshTickMsg:
		refresh := m.options.RefreshConfigs
		if refresh == nil {
			return m, nil
		}
		return m, func() tea.Msg {
			configs, err := refresh()
			return configRefreshedMsg{configs: configs, err: err}
		}
	case statusTickMsg:
		refresh := m.options.RefreshStatus
		if refresh == nil {
			return m, nil
		}
		return m, func() tea.Msg {
			return statusRefreshedMsg{summaries: refresh()}
		}
	case statusRefreshedMsg:
		m.applyRefreshedStatus(message.summaries)
		return m, m.nextStatusRefresh()
	case sudoCheckedMsg:
		if message.err == nil {
			m.sudo = sudoReady
			if m.sudoPending != nil {
				return m.resumeSudoPending()
			}
			return m, m.warmSudoCmd()
		}
		m.sudo = sudoMissing
		if m.sudoPending != nil {
			m.sudoPending = nil
			m.actionModal = nil
			m.errorMsg = "sudo is still unavailable (" + message.err.Error() + "); authenticate and start again"
		}
		return m, nil
	case sudoWarmTickMsg:
		check := m.options.SudoCheck
		if check == nil || m.sudo != sudoReady {
			return m, nil
		}
		return m, func() tea.Msg { return sudoWarmedMsg{err: check()} }
	case sudoWarmedMsg:
		if message.err == nil {
			return m, m.warmSudoCmd()
		}
		// The timestamp expired and could not be refreshed silently:
		// surface the need on the status bar; the next start offers auth.
		m.sudo = sudoMissing
		return m, nil
	case sudoAuthDoneMsg:
		if message.err != nil {
			m.sudo = sudoMissing
			m.sudoPending = nil
			m.actionModal = nil
			m.errorMsg = "sudo authentication failed: " + message.err.Error()
			return m, nil
		}
		// Re-verify instead of assuming: only a confirmed timestamp
		// resumes the parked action and starts the keep-warm ticker.
		return m, m.checkSudoCmd()
	case configRefreshedMsg:
		if message.err != nil {
			if m.errorMsg == "" {
				m.errorMsg = "config refresh: " + message.err.Error()
			}
			return m, m.nextConfigRefresh()
		}
		return m, m.applyRefreshedConfigs(message.configs)
	case rescanCandidatesMsg:
		if m.actionModal == nil || message.id != m.actionModal.id {
			return m, nil
		}
		if message.err != nil {
			m.actionModal.running = false
			m.actionModal.failed = true
			m.actionModal.title = "Rescan failed"
			m.actionModal.body = message.err.Error()
			return m, nil
		}
		if len(message.candidates) == 0 {
			m.actionModal.running = false
			m.actionModal.title = "No reachable pivots"
			m.actionModal.body = "No reachable instances were found in this connection's VPC."
			return m, nil
		}
		m.actionModal = nil
		m.rescan = &rescanSelector{connection: message.connection, candidates: message.candidates}
		return m, nil
	case newConnectionStartMsg:
		if m.actionModal == nil || message.id != m.actionModal.id {
			return m, nil
		}
		if message.err != nil || len(message.data.Regions) == 0 {
			m.actionModal.running = false
			m.actionModal.failed = message.err != nil
			m.actionModal.title = "New connection unavailable"
			m.actionModal.body = "No enabled AWS regions were found."
			if message.err != nil {
				m.actionModal.body = message.err.Error()
			}
			return m, nil
		}
		m.actionModal = nil
		m.newWizard = &newConnectionWizard{step: "region", start: message.data}
		return m, nil
	case newCandidatesMsg:
		if m.actionModal == nil || message.id != m.actionModal.id || m.newWizard == nil {
			return m, nil
		}
		if message.err != nil || len(message.candidates) == 0 {
			m.actionModal.running = false
			m.actionModal.failed = message.err != nil
			m.actionModal.title = "No reachable pivots"
			m.actionModal.body = "No reachable pivot instances were found."
			if message.err != nil {
				m.actionModal.body = message.err.Error()
			}
			m.newWizard.step = "kind"
			return m, nil
		}
		m.actionModal = nil
		m.newWizard.region = message.region
		m.newWizard.kind = message.kind
		m.newWizard.candidates = message.candidates
		m.newWizard.step = "candidate"
		return m, nil
	case newRoutesMsg:
		if m.actionModal == nil || message.id != m.actionModal.id || m.newWizard == nil {
			return m, nil
		}
		if message.err != nil {
			m.actionModal.running = false
			m.actionModal.failed = true
			m.actionModal.title = "Route discovery failed"
			m.actionModal.body = message.err.Error()
			m.newWizard.step = "candidate"
			return m, nil
		}
		m.actionModal = nil
		m.newWizard.routes = message.routes
		m.newWizard.step = "routes"
		return m, nil
	case configLoadedMsg:
		if message.err != nil {
			m.errorMsg = "edit: " + message.err.Error()
			return m, nil
		}
		m.editor = newConnectionEditor(message.cfg, m.options.AWSProfiles, m.options.AWSRegions)
		m.editor.field = min(message.startField, len(m.editor.fields)-1)
		m.editor.input = m.editor.values[m.editor.field]
		return m, nil
	case logLineMsg:
		if m.logSession == nil {
			return m, nil
		}
		m.logLines = append(m.logLines, string(message))
		if len(m.logLines) > 500 {
			m.logLines = m.logLines[len(m.logLines)-500:]
		}
		m.logOffset = 0
		return m, m.nextLogMessage()
	case logDoneMsg:
		if m.logSession == nil {
			return m, nil
		}
		m.logDone = true
		m.logError = message.err
		return m, nil
	case modalDismissTickMsg:
		// Only clear the box if it is still this exact result: a newer
		// action has a different id, and failures/confirmations must
		// always wait for the user.
		if m.actionModal != nil && m.actionModal.id == message.id &&
			!m.actionModal.running && !m.actionModal.failed && m.actionModal.confirmAction == "" {
			m.actionModal = nil
		}
		return m, nil
	case actionFinishedMsg:
		if m.actionModal == nil || message.id != m.actionModal.id {
			return m, nil
		}
		m.actionModal.running = false
		if message.err != nil {
			m.actionModal.failed = true
			m.actionModal.title = "Action failed"
			m.actionModal.body = message.err.Error()
			if message.result.Refresh && m.options.RefreshConnection != nil {
				refresh, name, id := m.options.RefreshConnection, message.connection, message.id
				return m, func() tea.Msg {
					var summary ConnSummary
					var detail *ConnDetail
					var err error
					func() {
						defer func() {
							if recovered := recover(); recovered != nil {
								err = fmt.Errorf("refreshing connection: %v", recovered)
							}
						}()
						summary, detail, err = refresh(name)
					}()
					return connectionRefreshedMsg{id: id, name: name, summary: summary, detail: detail, err: err}
				}
			}
			return m, nil
		}
		m.actionModal.title = message.result.Title
		m.actionModal.body = message.result.Body
		if message.newConnection && message.savedConfig != nil {
			cfg := message.savedConfig
			summary := summaryFromConfig(cfg, ConnSummary{})
			summary.Reachable = true
			m.options.Summaries = append(m.options.Summaries, summary)
			m.knownConfigs = append(m.knownConfigs, cfg)
			m.refreshHomeTable(false)
			if m.options.LoadDetail != nil {
				name, loader := cfg.Name, m.options.LoadDetail
				m.loadingDetail = name
				return m, func() tea.Msg {
					detail, err := loader(name)
					return detailLoadedMsg{name: name, detail: detail, err: err}
				}
			}
			return m, m.scheduleModalDismiss()
		}
		if message.result.RemoveConnection {
			filtered := m.options.Summaries[:0]
			for _, summary := range m.options.Summaries {
				if summary.Name != message.connection {
					filtered = append(filtered, summary)
				}
			}
			m.options.Summaries = filtered
			m.options.Level = 0
			m.options.Detail = nil
			m.refreshHomeTable(false)
			return m, m.scheduleModalDismiss()
		}
		if message.result.Refresh && m.options.RefreshConnection != nil {
			refresh, name, id := m.options.RefreshConnection, message.connection, message.id
			return m, func() tea.Msg {
				var summary ConnSummary
				var detail *ConnDetail
				var err error
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							err = fmt.Errorf("refreshing connection: %v", recovered)
						}
					}()
					summary, detail, err = refresh(name)
				}()
				return connectionRefreshedMsg{id: id, name: name, summary: summary, detail: detail, err: err}
			}
		}
		if message.savedConfig != nil {
			m.applySavedConfig(message.originalName, message.savedConfig, message.reachabilityKnown, message.targetReachable)
			if m.options.Level == 1 && m.options.Detail != nil && m.options.Detail.Name == message.originalName && m.options.LoadDetail != nil {
				name, loader := message.savedConfig.Name, m.options.LoadDetail
				m.loadingDetail = name
				return m, func() tea.Msg {
					detail, err := loader(name)
					return detailLoadedMsg{name: name, detail: detail, err: err}
				}
			}
		}
		return m, m.scheduleModalDismiss()
	case connectionRefreshedMsg:
		if message.err != nil {
			if m.actionModal != nil && message.id == m.actionModal.id {
				m.actionModal.title = "Action completed; refresh failed"
				m.actionModal.body += "\n" + message.err.Error()
			} else {
				m.errorMsg = "connection refresh failed: " + message.err.Error()
			}
			return m, nil
		}
		for index := range m.options.Summaries {
			if m.options.Summaries[index].Name == message.name {
				m.options.Summaries[index] = message.summary
				break
			}
		}
		m.refreshHomeTable(false)
		if m.options.Level == 1 && m.options.Detail != nil && m.options.Detail.Name == message.name {
			m.options.Detail = message.detail
		}
		return m, m.scheduleModalDismiss()
	case detailLoadedMsg:
		if message.name != m.loadingDetail {
			return m, nil
		}
		m.loadingDetail = ""
		if message.err != nil {
			m.errorMsg = "open " + message.name + ": " + message.err.Error()
			return m, nil
		}
		if message.detail == nil {
			m.errorMsg = "open " + message.name + ": connection is no longer available"
			return m, nil
		}
		m.options.Level = 1
		m.options.Detail = message.detail
		m.outcome.Action = ""
		m.outcome.Connection = message.name
		m.outcome.View = ViewConnection
		return m, m.scheduleModalDismiss()
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		m.home.SetWidth(max(message.Width, 40))
		// Table photos: the table renders its own header line on top of
		// the height rows, so reserve one line for it.
		m.home.SetHeight(max(m.chrome().Budget(m.height)-1, 3))
		m.refreshHomeTable(false)
		return m, nil
	case tea.KeyMsg:
		key := message.String()
		if m.searching {
			return m.updateSearch(key, message.Runes)
		}
		if m.actionModal != nil {
			if m.actionModal.confirmAction != "" {
				if m.actionModal.confirmAction == sudoAuthConfirm {
					if key == "esc" || key == "n" {
						m.sudoPending = nil
						m.actionModal = nil
						return m, nil
					}
					if key == "enter" || key == "y" {
						if m.options.SudoAuthenticate == nil {
							m.sudoPending = nil
							m.actionModal = nil
							m.errorMsg = "sudo is needed but cannot be prompted; run `sudo -v` before opening the browser"
							return m, nil
						}
						m.actionModal = nil
						return m, m.authenticateSudoCmd()
					}
					return m, nil
				}
				if key == "esc" || key == "n" {
					m.actionModal = nil
					return m, nil
				}
				if key == "enter" || key == "y" {
					action, connection := m.actionModal.confirmAction, m.actionModal.confirmConnection
					handler := m.options.Actions[action]
					if handler == nil {
						m.actionModal = nil
						m.errorMsg = "action is unavailable"
						return m, nil
					}
					m.actionID++
					id := m.actionID
					m.actionModal = &actionModal{id: id, title: actionLabel(action), body: "Working…", running: true}
					return m, func() tea.Msg {
						result, err := runActionSafely(handler, connection)
						return actionFinishedMsg{id: id, result: result, err: err, connection: connection}
					}
				}
				return m, nil
			}
			if !m.actionModal.running && (key == "enter" || key == "esc" || key == "q") {
				m.actionModal = nil
			}
			return m, nil
		}
		if m.errorMsg != "" {
			m.errorMsg = ""
		}
		if m.help {
			// The help overlay swallows exactly one key to dismiss itself;
			// this covers esc / enter / ? / q without triggering quits.
			m.help = false
			return m, nil
		}
		if m.shell {
			return m.updateShell(key, message.Runes)
		}
		if key == "/" {
			m.beginSearch()
			return m, nil
		}
		if m.editor != nil {
			return m.updateEditor(key, message.Runes)
		}
		if m.rescan != nil {
			return m.updateRescan(key)
		}
		if m.newWizard != nil {
			return m.updateNewWizard(key)
		}
		if m.logSession != nil {
			return m.updateLogs(key)
		}
		if m.loadingDetail != "" && key != "q" && key != "ctrl+c" {
			return m, nil
		}
		switch key {
		case "q", "ctrl+c":
			m.outcome.Quit = true
			return m, tea.Quit
		case "?":
			m.help = true
			return m, nil
		case ":":
			m.shell = true
			m.shellInput = ""
			m.shellIndex = 0
			return m, nil
		case "esc":
			if m.level() == 1 {
				name := m.currentName()
				m.options.Level = 0
				m.options.Detail = nil
				m.outcome.Action = ""
				m.outcome.Connection = name
				m.outcome.View = ViewHome
				return m, nil
			}
			// At home there is nothing to go back to.
			m.outcome.Quit = true
			return m, tea.Quit
		}
		if m.level() == 1 {
			return m.updateConnectionLevel(message, key)
		}
		return m.updateHomeLevel(message, key)
	}
	return m, nil
}

func (m *appModel) applySavedConfig(originalName string, cfg *config.Config, reachabilityKnown, targetReachable bool) {
	for index := range m.options.Summaries {
		summary := &m.options.Summaries[index]
		if summary.Name != originalName {
			continue
		}
		summary.Name = cfg.Name
		summary.Account = cfg.AccountID
		summary.Kind = string(cfg.Kind)
		summary.Method = cfg.ConnectionMethod
		summary.Region = cfg.Region
		summary.AWSProfile = cfg.AWSProfile
		if reachabilityKnown {
			summary.Reachable = targetReachable
			summary.Unreachable = !targetReachable
		}
		summary.Networks = strings.Join(cfg.SelectedCIDRs(), ", ")
		summary.DNS = "off"
		if cfg.DNS != nil && cfg.DNS.EffectiveMode() != config.DNSModeOff {
			summary.DNS = cfg.DNSSummary()
		}
		break
	}
	updated := make([]*config.Config, 0, len(m.knownConfigs)+1)
	for _, known := range m.knownConfigs {
		if known.Name != originalName || known.AccountID != cfg.AccountID {
			updated = append(updated, known)
		}
	}
	m.knownConfigs = append(updated, cfg)
	m.refreshHomeTable(false)
}

func (m appModel) nextConfigRefresh() tea.Cmd {
	if m.options.RefreshConfigs == nil {
		return nil
	}
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return configRefreshTickMsg{} })
}

// nextStatusRefresh schedules the next status probe; each probe schedules the following one, so probes never overlap.
func (m appModel) nextStatusRefresh() tea.Cmd {
	return tea.Tick(statusRefreshInterval, func(time.Time) tea.Msg { return statusTickMsg{} })
}

const statusRefreshInterval = 30 * time.Second

// applyRefreshedStatus copies live running and reachability state onto matching home rows.
func (m *appModel) applyRefreshedStatus(fresh []ConnSummary) {
	if len(fresh) == 0 {
		return
	}
	byKey := make(map[string]ConnSummary, len(fresh))
	for _, summary := range fresh {
		byKey[configKey(summary.Account, summary.Name)] = summary
	}
	for index := range m.options.Summaries {
		summary := &m.options.Summaries[index]
		live, ok := byKey[configKey(summary.Account, summary.Name)]
		if !ok {
			continue
		}
		summary.Running = live.Running
		summary.PID = live.PID
		summary.Stale = live.Stale
		summary.Reachable = live.Reachable
		summary.Unreachable = live.Unreachable
	}
	m.refreshHomeTable(false)
}

func configKey(accountID, name string) string { return accountID + "\x00" + name }

func summaryFromConfig(cfg *config.Config, previous ConnSummary) ConnSummary {
	previous.Account = cfg.AccountID
	previous.Name = cfg.Name
	previous.Kind = string(cfg.Kind)
	previous.Method = cfg.ConnectionMethod
	previous.Region = cfg.Region
	previous.AWSProfile = cfg.AWSProfile
	previous.Networks = strings.Join(cfg.SelectedCIDRs(), ", ")
	previous.DNS = "off"
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() != config.DNSModeOff {
		previous.DNS = cfg.DNSSummary()
	}
	return previous
}

func (m *appModel) applyRefreshedConfigs(configs []*config.Config) tea.Cmd {
	oldByKey := make(map[string]*config.Config, len(m.knownConfigs))
	newByKey := make(map[string]*config.Config, len(configs))
	for _, cfg := range m.knownConfigs {
		oldByKey[configKey(cfg.AccountID, cfg.Name)] = cfg
	}
	for _, cfg := range configs {
		newByKey[configKey(cfg.AccountID, cfg.Name)] = cfg
	}
	changed := make(map[string]bool)
	for key, cfg := range newByKey {
		changed[key] = !reflect.DeepEqual(oldByKey[key], cfg)
	}
	for key := range oldByKey {
		if _, exists := newByKey[key]; !exists {
			changed[key] = true
		}
	}

	if m.editor != nil {
		for key, isChanged := range changed {
			if isChanged && strings.HasSuffix(key, "\x00"+m.editor.originalName) {
				m.editor.conflicted = true
				m.errorMsg = "connection changed on disk; cancel and reopen edit before saving"
				break
			}
		}
	}

	previous := make(map[string]ConnSummary, len(m.options.Summaries))
	for _, summary := range m.options.Summaries {
		previous[configKey(summary.Account, summary.Name)] = summary
	}
	summaries := make([]ConnSummary, 0, len(configs))
	for _, cfg := range configs {
		summary := summaryFromConfig(cfg, previous[configKey(cfg.AccountID, cfg.Name)])
		summaries = append(summaries, summary)
	}
	m.options.Summaries = summaries
	m.knownConfigs = configs
	m.refreshHomeTable(false)

	if m.level() != 1 || m.options.Detail == nil {
		return m.nextConfigRefresh()
	}
	name := m.options.Detail.Name
	var current *config.Config
	for _, cfg := range configs {
		if cfg.Name == name {
			current = cfg
			break
		}
	}
	if current == nil {
		m.options.Level = 0
		m.options.Detail = nil
		m.errorMsg = "connection removed from config"
		return m.nextConfigRefresh()
	}
	key := configKey(current.AccountID, current.Name)
	if changed[key] && m.options.LoadDetail != nil {
		loader := m.options.LoadDetail
		m.loadingDetail = name
		return tea.Batch(m.nextConfigRefresh(), func() tea.Msg {
			detail, err := loader(name)
			return detailLoadedMsg{name: name, detail: detail, err: err}
		})
	}
	return m.nextConfigRefresh()
}

func (m appModel) activeSearchScope() string {
	switch {
	case m.logSession != nil:
		return searchLogs
	case m.newWizard != nil && m.newWizard.step == "region":
		return searchNewRegions
	case m.newWizard != nil && m.newWizard.step == "kind":
		return searchNewKinds
	case m.newWizard != nil && m.newWizard.step == "candidate":
		return searchNewCandidates
	case m.newWizard != nil && m.newWizard.step == "routes":
		return searchNewRoutes
	case m.editor != nil && m.editor.routePicker:
		return searchEditorRoutes
	case m.rescan != nil:
		return searchRescan
	case m.editor != nil && len(m.editor.choices) > 0:
		return searchEditorChoices
	case m.editor != nil:
		return searchEditor
	case m.level() == 1:
		return searchDetail
	default:
		return searchHome
	}
}

func (m appModel) searchQuery(scope string) string {
	if m.searching && m.searchTarget == scope {
		return strings.TrimSpace(m.searchInput)
	}
	if scope == searchHome {
		return strings.TrimSpace(m.searchFilter)
	}
	return strings.TrimSpace(m.searchFilters[scope])
}

func (m *appModel) beginSearch() {
	m.searchTarget = m.activeSearchScope()
	m.searchOriginal = m.searchQuery(m.searchTarget)
	m.searchInput = m.searchOriginal
	m.searching = true
}

func (m appModel) updateSearch(key string, runes []rune) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.searching = false
		m.searchInput = m.searchOriginal
	case "enter":
		query := strings.TrimSpace(m.searchInput)
		if m.searchTarget == searchHome {
			m.searchFilter = query
		} else {
			if m.searchFilters == nil {
				m.searchFilters = make(map[string]string)
			}
			m.searchFilters[m.searchTarget] = query
		}
		m.searching = false
		if m.searchTarget == searchHome {
			m.refreshHomeTable(true)
		}
		if m.searchTarget == searchLogs {
			m.logOffset = 0
		}
		switch m.searchTarget {
		case searchEditor:
			if m.editor != nil {
				if indices := matchingEditorFields(m.editor, query); len(indices) > 0 {
					m.editor.field = indices[0]
					m.editor.input = m.editor.values[indices[0]]
				}
			}
		case searchEditorChoices:
			if m.editor != nil {
				if indices := matchingChoiceIndices(m.editor.choices, query); len(indices) > 0 {
					m.editor.choiceCursor = indices[0]
				}
			}
		case searchEditorRoutes:
			if m.editor != nil {
				if indices := matchingEditorRoutes(m.editor.routes, query); len(indices) > 0 {
					m.editor.routeCursor = indices[0]
				}
			}
		case searchRescan:
			if m.rescan != nil {
				if indices := matchingRescanCandidates(m.rescan, query); len(indices) > 0 {
					m.rescan.cursor = indices[0]
				}
			}
		case searchNewRegions:
			if m.newWizard != nil {
				if indices := matchingChoiceIndices(m.newWizard.start.Regions, query); len(indices) > 0 {
					m.newWizard.cursor = indices[0]
				}
			}
		case searchNewKinds:
			if m.newWizard != nil {
				if indices := matchingChoiceIndices(newConnectionKinds, query); len(indices) > 0 {
					m.newWizard.cursor = indices[0]
				}
			}
		case searchNewCandidates:
			if m.newWizard != nil {
				if indices := matchingNewCandidates(m.newWizard, query); len(indices) > 0 {
					m.newWizard.cursor = indices[0]
				}
			}
		case searchNewRoutes:
			if m.newWizard != nil {
				if indices := matchingNewRoutes(m.newWizard, query); len(indices) > 0 {
					m.newWizard.cursor = indices[0]
				}
			}
		}
	case "backspace":
		input := []rune(m.searchInput)
		if len(input) > 0 {
			m.searchInput = string(input[:len(input)-1])
			if m.searchTarget == searchHome {
				m.refreshHomeTable(true)
			}
			if m.searchTarget == searchLogs {
				m.logOffset = 0
			}
		}
	default:
		if len(runes) > 0 {
			m.searchInput += string(runes)
			if m.searchTarget == searchHome {
				m.refreshHomeTable(true)
			}
			if m.searchTarget == searchLogs {
				m.logOffset = 0
			}
		}
	}
	return m, nil
}

func (m appModel) visibleSummaries() []ConnSummary {
	query := m.searchQuery(searchHome)
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return m.options.Summaries
	}
	var visible []ConnSummary
	for _, summary := range m.options.Summaries {
		searchable := strings.ToLower(strings.Join([]string{
			summary.Name, summary.Account, summary.Kind, summary.Method,
			summary.Region, summary.AWSProfileLabel(), summary.Networks, summary.DNS, summary.StatusLabel(),
		}, " "))
		if strings.Contains(searchable, query) {
			visible = append(visible, summary)
		}
	}
	return visible
}

func (m *appModel) refreshHomeTable(resetCursor bool) {
	cursor := m.home.Cursor()
	if resetCursor {
		cursor = 0
	}
	visible := m.visibleSummaries()
	width := m.width
	if width <= 0 {
		width = 120
	}
	columns, rows := homeLayout(visible, width)
	safeSetColumnsRows(&m.home, columns, rows)
	if cursor >= len(visible) {
		cursor = max(len(visible)-1, 0)
	}
	m.home.SetCursor(cursor)
}

func (m appModel) updateHomeLevel(message tea.Msg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "enter":
		if len(m.visibleSummaries()) == 0 {
			m.errorMsg = "no saved connections yet — press n to scan for one"
			return m, nil
		}
		m.outcome.Action = ActOpen
		return m.openConnection(m.currentName())
	case "n":
		return m.requestAction(ActNew, "", ViewHome)
	case "s", "x", "l", "i", "r", "f", "d", "e", "a", "v":
		return m.fireHomeAction(key)
	}
	var command tea.Cmd
	m.home, command = m.home.Update(message)
	return m, command
}

func (m appModel) fireHomeAction(key string) (tea.Model, tea.Cmd) {
	name := m.currentName()
	if name == "" {
		m.errorMsg = "no saved connections yet — press n to scan for one"
		return m, nil
	}
	var action string
	switch key {
	case "s":
		action = ActStart
	case "x":
		action = ActStop
	case "l":
		action = ActLogs
	case "i":
		action = ActStatus
	case "r":
		action = ActRescan
	case "f":
		action = ActForget
	case "d":
		action = ActDNSEdit
	case "e":
		action = ActEdit
	case "a":
		action = ActLaunch
	case "v":
		action = ActAWSCheck
	}
	if action == "" {
		return m, nil
	}
	return m.requestAction(action, name, ViewHome)
}

// OfflineBlocks reports whether an action needs AWS connectivity.
func OfflineBlocks(action string) bool {
	return action == ActNew || action == ActRescan
}

// requestAction records the next action for the driver, refusing
// network-requiring actions while offline.
func (m appModel) requestAction(action, connection, view string) (tea.Model, tea.Cmd) {
	if action == ActOpen {
		return m.openConnection(connection)
	}
	if m.options.Offline && OfflineBlocks(action) {
		m.errorMsg = "offline — " + action + " needs AWS connectivity (check network / credentials)"
		return m, nil
	}
	if m.guardSudo(action, connection, view) {
		return m, nil
	}
	if action == ActForget || action == ActDNSTeardown {
		m.actionID++
		m.actionModal = &actionModal{
			id: m.actionID, title: "Confirm " + actionLabel(action),
			body:          "This operation changes connection or resolver state. Press Enter to continue or Esc to cancel.",
			confirmAction: action, confirmConnection: connection,
		}
		return m, nil
	}
	if action == ActLogs && m.options.StartLogs != nil {
		m.shell = false
		m.shellInput = ""
		m.outcome.Action = ""
		session, err := m.options.StartLogs(connection)
		if err != nil {
			m.errorMsg = "logs: " + err.Error()
			return m, nil
		}
		m.logSession = session
		m.logLines = nil
		m.logOffset = 0
		m.logDone = false
		m.logError = nil
		m.logTitle = "Live sshuttle logs"
		return m, m.nextLogMessage()
	}
	if action == ActNew && m.options.StartNew != nil {
		m.actionID++
		id := m.actionID
		m.actionModal = &actionModal{id: id, title: "Loading AWS regions", body: "Checking enabled regions…", running: true}
		start := m.options.StartNew
		return m, func() tea.Msg {
			data, err := start()
			return newConnectionStartMsg{id: id, data: data, err: err}
		}
	}
	if action == ActLaunch && m.options.StartLaunch != nil {
		m.outcome.Action = ""
		session, err := m.options.StartLaunch(connection)
		if err != nil {
			m.errorMsg = "launch: " + err.Error()
			return m, nil
		}
		m.logSession = session
		m.logLines = nil
		m.logOffset = 0
		m.logDone = false
		m.logError = nil
		m.logTitle = "Foreground tunnel output"
		return m, m.nextLogMessage()
	}
	if action == ActRescan && m.options.PrepareRescan != nil {
		m.actionID++
		id := m.actionID
		m.actionModal = &actionModal{id: id, title: "Scanning VPC", body: "Checking reachable pivot instances…", running: true}
		m.outcome.Action = ""
		prepare := m.options.PrepareRescan
		return m, func() tea.Msg {
			candidates, err := prepare(connection)
			return rescanCandidatesMsg{id: id, connection: connection, candidates: candidates, err: err}
		}
	}
	if (action == ActEdit || action == ActDNSEdit) && m.options.LoadConfig != nil {
		loader := m.options.LoadConfig
		startField := 0
		if action == ActDNSEdit {
			startField = 13
		}
		return m, func() tea.Msg {
			var cfg *config.Config
			var loadErr error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						loadErr = fmt.Errorf("loading connection: %v", recovered)
					}
				}()
				cfg, loadErr = loader(connection)
			}()
			return configLoadedMsg{cfg: cfg, err: loadErr, startField: startField}
		}
	}
	if handler := m.options.Actions[action]; handler != nil {
		m.actionID++
		id := m.actionID
		m.actionModal = &actionModal{id: id, title: actionLabel(action), body: "Working…", running: true}
		m.shell = false
		m.shellInput = ""
		m.outcome.Action = ""
		return m, func() tea.Msg {
			result, err := runActionSafely(handler, connection)
			return actionFinishedMsg{id: id, result: result, err: err, connection: connection}
		}
	}
	m.outcome.Action = action
	m.outcome.Connection = connection
	m.outcome.View = view
	return m, tea.Quit
}

func runActionSafely(handler ActionHandler, connection string) (result ActionResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("action failed: %v", recovered)
		}
	}()
	return handler(connection)
}

func newConnectionEditor(cfg *config.Config, profiles []string, regionOptions ...[]string) *connectionEditor {
	routes := append([]config.NetworkRoute(nil), cfg.Routes...)
	if len(routes) == 0 {
		for _, cidr := range cfg.VpcCIDRs {
			routes = append(routes, config.NetworkRoute{CIDR: cidr, Source: "pivot_vpc", Selected: true})
		}
	}
	values := []string{cfg.Name, string(cfg.Kind), cfg.ConnectionMethod, cfg.Region, cfg.Target, cfg.SSHUser, cfg.SSHKeyPath, fmt.Sprint(cfg.SSHPort), cfg.VpcID, cfg.SubnetID, cfg.InstanceID, cfg.NameTag, editorRoutesLabel(routes)}
	dns := cfg.DNS
	mode, resolvers, domains, toNS, manage := "off", "", "", "", "false"
	if dns != nil {
		effective := cfg.EffectiveDNSConfig()
		mode, resolvers, domains, toNS, manage = string(effective.Mode), strings.Join(effective.Resolver, ","), strings.Join(effective.Domains, ","), effective.ToNS, fmt.Sprint(effective.ManageResolver)
	}
	values = append(values, mode, resolvers, domains, toNS, manage)
	profile := cfg.AWSProfile
	if profile == "" {
		profile = "default"
	}
	values = append(values, profile)
	if len(profiles) == 0 {
		profiles = []string{"default"}
	}
	profileKnown := false
	for _, candidate := range profiles {
		if candidate == profile {
			profileKnown = true
			break
		}
	}
	if !profileKnown {
		profiles = append(profiles, profile)
	}
	var regions []string
	if len(regionOptions) > 0 {
		regions = append(regions, regionOptions[0]...)
	}
	regionKnown := false
	for _, candidate := range regions {
		if candidate == cfg.Region {
			regionKnown = true
			break
		}
	}
	if !regionKnown && cfg.Region != "" {
		regions = append(regions, cfg.Region)
	}
	return &connectionEditor{cfg: cfg, originalName: cfg.Name, fields: []string{"name", "kind", "method", "region", "target", "ssh user", "ssh key", "ssh port", "vpc id", "subnet id", "instance id", "name tag", "routes", "dns mode", "dns resolvers", "dns domains", "dns to-ns", "manage resolver", "aws profile"}, values: values, profiles: profiles, regions: regions, routes: routes}
}

func editorRoutesLabel(routes []config.NetworkRoute) string {
	var selected []string
	for _, route := range routes {
		if route.Selected {
			selected = append(selected, route.CIDR)
		}
	}
	return fmt.Sprintf("%d/%d selected", len(selected), len(routes))
}

func (m appModel) updateEditor(key string, runes []rune) (tea.Model, tea.Cmd) {
	e := m.editor
	if e.routePicker {
		if e.addingRoute {
			switch key {
			case "esc":
				e.addingRoute = false
				e.routeInput = ""
				e.routeError = ""
			case "backspace":
				if len(e.routeInput) > 0 {
					e.routeInput = e.routeInput[:len(e.routeInput)-1]
				}
			case "enter":
				prefix, err := netip.ParsePrefix(strings.TrimSpace(e.routeInput))
				if err != nil {
					e.routeError = "enter a valid CIDR, e.g. 10.20.0.0/16"
					return m, nil
				}
				cidr := prefix.Masked().String()
				found := false
				for index := range e.routes {
					if e.routes[index].CIDR == cidr {
						e.routes[index].Selected = true
						e.routeCursor = index
						found = true
						break
					}
				}
				if !found {
					e.routes = append(e.routes, config.NetworkRoute{CIDR: cidr, Source: "manual", Selected: true})
					e.routeCursor = len(e.routes) - 1
				}
				e.routeInput = ""
				e.routeError = ""
				e.addingRoute = false
			default:
				if len(runes) > 0 {
					e.routeInput += string(runes)
					e.routeError = ""
				}
			}
			return m, nil
		}
		indices := matchingEditorRoutes(e.routes, m.searchQuery(searchEditorRoutes))
		position := indexOf(indices, e.routeCursor)
		switch key {
		case "esc":
			e.routes = e.routeOriginal
			e.routePicker = false
		case "up", "k":
			if position > 0 {
				e.routeCursor = indices[position-1]
			}
		case "down", "j":
			if position >= 0 && position < len(indices)-1 {
				e.routeCursor = indices[position+1]
			}
		case " ":
			if position >= 0 {
				e.routes[e.routeCursor].Selected = !e.routes[e.routeCursor].Selected
			}
		case "a", "A":
			allSelected := len(indices) > 0
			for _, index := range indices {
				allSelected = allSelected && e.routes[index].Selected
			}
			for _, index := range indices {
				e.routes[index].Selected = !allSelected
			}
		case "m":
			e.addingRoute = true
			e.routeInput = ""
			e.routeError = ""
		case "enter":
			e.routePicker = false
			e.routeOriginal = nil
			e.values[12] = editorRoutesLabel(e.routes)
		}
		return m, nil
	}
	if len(e.choices) > 0 {
		indices := matchingChoiceIndices(e.choices, m.searchQuery(searchEditorChoices))
		position := indexOf(indices, e.choiceCursor)
		switch key {
		case "esc":
			e.choices = nil
		case "up", "k":
			if position > 0 {
				e.choiceCursor = indices[position-1]
			}
		case "down", "j":
			if position >= 0 && position < len(indices)-1 {
				e.choiceCursor = indices[position+1]
			}
		case "enter":
			if position >= 0 {
				e.values[e.field] = e.choices[e.choiceCursor]
				e.input = e.values[e.field]
				e.choices = nil
			}
		}
		return m, nil
	}
	switch key {
	case "esc":
		m.editor = nil
	case "up":
		e.field = neighboringEditorField(e, m.searchQuery(searchEditor), -1)
	case "down", "tab":
		e.field = neighboringEditorField(e, m.searchQuery(searchEditor), 1)
	case "left", "right":
		if e.field == len(e.fields)-1 && len(e.profiles) > 0 {
			index := 0
			for i, profile := range e.profiles {
				if profile == e.values[e.field] {
					index = i
					break
				}
			}
			if key == "left" {
				index = (index + len(e.profiles) - 1) % len(e.profiles)
			} else {
				index = (index + 1) % len(e.profiles)
			}
			e.values[e.field] = e.profiles[index]
			e.input = e.values[e.field]
		}
	case "v":
		if m.options.CheckAWSProfile != nil {
			cfg, err := editorConfig(e)
			if err != nil {
				m.errorMsg = "profile check: " + err.Error()
				return m, nil
			}
			checker, profile := m.options.CheckAWSProfile, e.values[len(e.values)-1]
			m.actionID++
			id := m.actionID
			m.actionModal = &actionModal{id: id, title: "Checking AWS profile", body: "Working…", running: true}
			return m, func() tea.Msg {
				result, err := checker(cfg, profile)
				return actionFinishedMsg{id: id, result: result, err: err}
			}
		}
	case "backspace":
		if len(e.input) > 0 {
			e.input = e.input[:len(e.input)-1]
			e.values[e.field] = e.input
		}
	case "enter":
		if e.fields[e.field] == "routes" {
			e.routePicker = true
			e.routeOriginal = append([]config.NetworkRoute(nil), e.routes...)
			e.routeCursor = firstSelectedRoute(e.routes)
			return m, nil
		}
		if choices := editorFieldChoices(e); len(choices) > 0 {
			e.choices = choices
			e.choiceCursor = 0
			for index, choice := range choices {
				if choice == e.values[e.field] {
					e.choiceCursor = index
					break
				}
			}
			return m, nil
		}
		if e.field < len(e.fields)-1 {
			e.field++
			e.input = e.values[e.field]
			break
		}
		m.errorMsg = "press ctrl+s to save the connection"
	case "ctrl+s":
		if e.conflicted {
			m.errorMsg = "connection changed on disk; cancel and reopen edit before saving"
			return m, nil
		}
		if e.isNew && m.options.SaveNewConfig == nil || !e.isNew && m.options.SaveConfig == nil {
			m.errorMsg = "edit: saving is unavailable"
			return m, nil
		}
		cfg, err := editorConfig(e)
		if err != nil {
			m.errorMsg = "edit: " + err.Error()
			return m, nil
		}
		saver, name := m.options.SaveConfig, e.originalName
		if e.isNew {
			name = cfg.Name
			saver = func(_ string, draft *config.Config) error { return m.options.SaveNewConfig(draft) }
		}
		m.editor = nil
		m.actionID++
		id := m.actionID
		m.actionModal = &actionModal{id: id, title: "Saving connection", body: "Working…", running: true}
		return m, func() tea.Msg {
			var saveErr error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						if failure, ok := recovered.(error); ok {
							saveErr = failure
						} else {
							saveErr = fmt.Errorf("saving connection: %v", recovered)
						}
					}
				}()
				saveErr = saver(name, cfg)
			}()
			return actionFinishedMsg{
				id:            id,
				result:        ActionResult{Title: "Connection saved", Body: cfg.Name},
				err:           saveErr,
				savedConfig:   cfg,
				originalName:  name,
				newConnection: e.isNew,
			}
		}
	default:
		if len(runes) > 0 && len(editorFieldChoices(e)) == 0 {
			e.input += string(runes)
			e.values[e.field] = e.input
		}
	}
	if m.editor != nil {
		e.input = e.values[e.field]
	}
	return m, nil
}

func editorFieldChoices(editor *connectionEditor) []string {
	switch editor.fields[editor.field] {
	case "region":
		return append([]string(nil), editor.regions...)
	case "kind":
		return []string{string(config.ProfileKindVPC), string(config.ProfileKindDedicated)}
	case "method":
		return []string{"ssm", "direct_ip"}
	case "dns mode":
		return []string{"off", "all", "split"}
	case "manage resolver":
		return []string{"false", "true"}
	case "aws profile":
		return append([]string(nil), editor.profiles...)
	default:
		return nil
	}
}

func (m appModel) updateRescan(key string) (tea.Model, tea.Cmd) {
	selector := m.rescan
	indices := matchingRescanCandidates(selector, m.searchQuery(searchRescan))
	position := indexOf(indices, selector.cursor)
	switch key {
	case "esc", "q":
		m.rescan = nil
	case "up", "k":
		if position > 0 {
			selector.cursor = indices[position-1]
		}
	case "down", "j":
		if position >= 0 && position < len(indices)-1 {
			selector.cursor = indices[position+1]
		}
	case "enter":
		if m.options.SaveRescan == nil {
			m.errorMsg = "rescan: saving is unavailable"
			return m, nil
		}
		if position < 0 {
			m.errorMsg = "rescan: no candidates match the search"
			return m, nil
		}
		selected := selector.candidates[selector.cursor]
		connection, save := selector.connection, m.options.SaveRescan
		m.rescan = nil
		m.actionID++
		id := m.actionID
		m.actionModal = &actionModal{id: id, title: "Saving rescan", body: "Updating connection YAML…", running: true}
		return m, func() tea.Msg {
			var cfg *config.Config
			var err error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						err = fmt.Errorf("saving rescanned connection: %v", recovered)
					}
				}()
				cfg, err = save(connection, selected)
			}()
			return actionFinishedMsg{
				id:                id,
				result:            ActionResult{Title: "Rescan complete", Body: fmt.Sprintf("%s now uses %s (%s).", connection, selected.InstanceID, selected.Method)},
				err:               err,
				savedConfig:       cfg,
				originalName:      connection,
				reachabilityKnown: true,
				targetReachable:   selected.Reachable,
			}
		}
	}
	return m, nil
}

var newConnectionKinds = []string{string(config.ProfileKindVPC), string(config.ProfileKindDedicated)}

func matchingNewCandidates(wizard *newConnectionWizard, query string) []int {
	selector := &rescanSelector{candidates: wizard.candidates}
	return matchingRescanCandidates(selector, query)
}

func matchingNewRoutes(wizard *newConnectionWizard, query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	indices := make([]int, 0, len(wizard.routes))
	for index, route := range wizard.routes {
		searchable := strings.ToLower(strings.Join([]string{route.CIDR, route.Source, route.TargetType, route.TargetID, route.Description}, " "))
		if query == "" || strings.Contains(searchable, query) {
			indices = append(indices, index)
		}
	}
	return indices
}

func (m appModel) updateNewWizard(key string) (tea.Model, tea.Cmd) {
	wizard := m.newWizard
	query := m.searchQuery(m.activeSearchScope())
	switch wizard.step {
	case "region":
		indices := matchingChoiceIndices(wizard.start.Regions, query)
		position := indexOf(indices, wizard.cursor)
		switch key {
		case "esc", "q":
			m.newWizard = nil
		case "up", "k":
			if position > 0 {
				wizard.cursor = indices[position-1]
			}
		case "down", "j":
			if position >= 0 && position < len(indices)-1 {
				wizard.cursor = indices[position+1]
			}
		case "enter":
			if position >= 0 {
				wizard.region = wizard.start.Regions[wizard.cursor]
				wizard.step = "kind"
				wizard.cursor = 0
			}
		}
	case "kind":
		indices := matchingChoiceIndices(newConnectionKinds, query)
		position := indexOf(indices, wizard.cursor)
		switch key {
		case "esc", "q":
			wizard.step = "region"
			wizard.cursor = 0
		case "up", "k":
			if position > 0 {
				wizard.cursor = indices[position-1]
			}
		case "down", "j":
			if position >= 0 && position < len(indices)-1 {
				wizard.cursor = indices[position+1]
			}
		case "enter":
			if position < 0 || m.options.ScanNew == nil {
				return m, nil
			}
			wizard.kind = newConnectionKinds[wizard.cursor]
			wizard.step = "scanning"
			m.actionID++
			id, region, kind := m.actionID, wizard.region, wizard.kind
			m.actionModal = &actionModal{id: id, title: "Scanning region", body: "Finding reachable pivots…", running: true}
			scan := m.options.ScanNew
			return m, func() tea.Msg {
				candidates, err := scan(region, kind)
				return newCandidatesMsg{id: id, region: region, kind: kind, candidates: candidates, err: err}
			}
		}
	case "candidate":
		indices := matchingNewCandidates(wizard, query)
		position := indexOf(indices, wizard.cursor)
		switch key {
		case "esc", "q":
			wizard.step = "kind"
			wizard.cursor = 0
		case "up", "k":
			if position > 0 {
				wizard.cursor = indices[position-1]
			}
		case "down", "j":
			if position >= 0 && position < len(indices)-1 {
				wizard.cursor = indices[position+1]
			}
		case "enter":
			if position < 0 || m.options.PrepareNewRoutes == nil {
				return m, nil
			}
			wizard.candidate = wizard.candidates[wizard.cursor]
			wizard.step = "routes-loading"
			m.actionID++
			id, region, kind, candidate := m.actionID, wizard.region, wizard.kind, wizard.candidate
			m.actionModal = &actionModal{id: id, title: "Loading routes", body: "Discovering network routes…", running: true}
			prepare := m.options.PrepareNewRoutes
			return m, func() tea.Msg {
				routes, err := prepare(region, kind, candidate)
				return newRoutesMsg{id: id, routes: routes, err: err}
			}
		}
	case "routes":
		indices := matchingNewRoutes(wizard, query)
		position := indexOf(indices, wizard.cursor)
		switch key {
		case "esc", "q":
			wizard.step = "candidate"
			wizard.cursor = 0
		case "up", "k":
			if position > 0 {
				wizard.cursor = indices[position-1]
			}
		case "down", "j":
			if position >= 0 && position < len(indices)-1 {
				wizard.cursor = indices[position+1]
			}
		case " ":
			if position >= 0 {
				wizard.routes[wizard.cursor].Selected = !wizard.routes[wizard.cursor].Selected
			}
		case "a", "A":
			allSelected := true
			for _, index := range indices {
				allSelected = allSelected && wizard.routes[index].Selected
			}
			for _, index := range indices {
				wizard.routes[index].Selected = !allSelected
			}
		case "enter":
			var selected []config.NetworkRoute
			for _, route := range wizard.routes {
				if route.Selected {
					selected = append(selected, route)
				}
			}
			if len(selected) == 0 {
				m.errorMsg = "select at least one route"
				return m, nil
			}
			candidate := wizard.candidate
			cfg := &config.Config{
				AccountID: wizard.start.AccountID, AWSProfile: wizard.start.AWSProfile,
				Kind: config.ProfileKind(wizard.kind), Region: wizard.region,
				VpcID: candidate.VpcID, VpcCIDRs: append([]string(nil), candidate.VpcCIDRs...),
				SubnetID: candidate.SubnetID, InstanceID: candidate.InstanceID, NameTag: candidate.Name,
				Routes: selected, ConnectionMethod: candidate.Method, Target: candidate.Target,
				SSHPort: wizard.start.SSHPort,
			}
			m.newWizard = nil
			m.editor = newConnectionEditor(cfg, m.options.AWSProfiles, m.options.AWSRegions)
			m.editor.isNew = true
			m.editor.originalName = ""
		}
	}
	return m, nil
}

func editorConfig(e *connectionEditor) (*config.Config, error) {
	cfg := *e.cfg
	cfg.Name, cfg.Kind, cfg.ConnectionMethod, cfg.Region, cfg.Target = e.values[0], config.ProfileKind(e.values[1]), e.values[2], e.values[3], e.values[4]
	cfg.SSHUser, cfg.SSHKeyPath = e.values[5], e.values[6]
	if _, err := fmt.Sscanf(e.values[7], "%d", &cfg.SSHPort); err != nil {
		return nil, fmt.Errorf("invalid SSH port %q", e.values[7])
	}
	cfg.VpcID, cfg.SubnetID, cfg.InstanceID, cfg.NameTag = e.values[8], e.values[9], e.values[10], e.values[11]
	cfg.Routes = append([]config.NetworkRoute(nil), e.routes...)
	cfg.AWSProfile = e.values[18]
	if e.values[13] == "off" || strings.TrimSpace(e.values[13]) == "" {
		cfg.DNS = nil
	} else {
		cfg.DNS = &config.DNSConfig{Mode: config.DNSMode(e.values[13]), Resolver: splitEditorList(e.values[14]), Domains: splitEditorList(e.values[15]), ToNS: strings.TrimSpace(e.values[16]), ManageResolver: strings.EqualFold(strings.TrimSpace(e.values[17]), "true")}
	}
	return &cfg, nil
}

func firstSelectedRoute(routes []config.NetworkRoute) int {
	for index, route := range routes {
		if route.Selected {
			return index
		}
	}
	return 0
}

func splitEditorList(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func (m appModel) nextLogMessage() tea.Cmd {
	if m.logSession == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case line, ok := <-m.logSession.Lines:
			if ok {
				return logLineMsg(line)
			}
		default:
		}
		select {
		case line, ok := <-m.logSession.Lines:
			if !ok {
				return logDoneMsg{err: <-m.logSession.Done}
			}
			return logLineMsg(line)
		case err := <-m.logSession.Done:
			return logDoneMsg{err: err}
		}
	}
}

func (m appModel) updateLogs(key string) (tea.Model, tea.Cmd) {
	if key == "esc" || key == "q" || key == "ctrl+c" {
		m.logSession.Cancel()
		m.logSession = nil
		m.logLines = nil
		m.logOffset = 0
		m.logTitle = ""
		return m, nil
	}
	visibleLines := filterAndHighlightLines(m.logLines, m.searchQuery(searchLogs))
	visible := max(m.chrome().Budget(m.height), 1)
	switch key {
	case "up":
		m.logOffset = min(m.logOffset+1, max(len(visibleLines)-visible, 0))
	case "down":
		m.logOffset = max(m.logOffset-1, 0)
	case "pgup":
		m.logOffset = min(m.logOffset+visible, max(len(visibleLines)-visible, 0))
	case "pgdown":
		m.logOffset = max(m.logOffset-visible, 0)
	}
	return m, nil
}

func actionLabel(action string) string {
	if action == "" {
		return "Action"
	}
	return strings.ToUpper(action[:1]) + action[1:]
}

func (m appModel) openConnection(name string) (tea.Model, tea.Cmd) {
	if m.options.LoadDetail == nil {
		m.errorMsg = "connection details are unavailable"
		return m, nil
	}
	m.loadingDetail = name
	m.shell = false
	m.shellInput = ""
	m.outcome.Action = ""
	m.outcome.Connection = name
	m.outcome.View = ViewHome
	loader := m.options.LoadDetail
	return m, func() tea.Msg {
		detail, err := loader(name)
		return detailLoadedMsg{name: name, detail: detail, err: err}
	}
}

func (m appModel) updateConnectionLevel(message tea.Msg, key string) (tea.Model, tea.Cmd) {
	detail := m.options.Detail
	var action string
	switch key {
	case "s":
		action = ActStart
	case "x":
		action = ActStop
	case "l":
		action = ActLogs
	case "i":
		action = ActStatus
	case "p":
		action = ActPrint
	case "r":
		action = ActRescan
	case "f":
		action = ActForget
	case "e":
		action = ActEdit
	case "d":
		action = ActDNSEdit
	case "a":
		action = ActLaunch
	case "v":
		action = ActAWSCheck
	case "1":
		action = ActDNSSetup
	case "2":
		action = ActDNSTeardown
	}
	if action == "" {
		return m, nil
	}
	return m.requestAction(action, detail.Name, ViewConnection)
}

// ---------------------------------------------------------------------------
// Shell (`:` command line)
// ---------------------------------------------------------------------------

var shellCommands = []string{
	"start", "stop", "launch", "logs", "status", "print",
	"rescan", "forget", "edit", "dns", "new", "help", "quit", "exit", "q",
}

func (m appModel) updateShell(key string, runes []rune) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.shell = false
		m.shellInput = ""
		return m, nil
	case "backspace":
		if n := len(m.shellInput); n > 0 {
			m.shellInput = m.shellInput[:n-1]
		}
		return m, nil
	case "tab":
		matches := m.matchingCommands()
		if len(matches) > 0 {
			m.shellIndex++
			m.shellInput = matches[m.shellIndex%len(matches)] + " "
		}
		return m, nil
	case "enter":
		outcome, err := ParseShell(m.shellInput, m.currentName(), m.connectionNames())
		if err != nil {
			m.errorMsg = err.Error()
			return m, nil
		}
		if outcome.Quit {
			m.outcome.Quit = true
			return m, tea.Quit
		}
		if outcome.Action == "" {
			// help / bare command name: stay in the app, list the options.
			m.errorMsg = "commands: " + strings.Join(shellCommands, ", ")
			return m, nil
		}
		return m.requestAction(outcome.Action, outcome.Connection, outcome.View)
	}
	// Any unhandled key carrying runes (bubbletea normalizes printable
	// characters this way) extends the command line.
	if len(runes) > 0 {
		m.shellInput += string(runes)
	}
	return m, nil
}

func (m appModel) connectionNames() []string {
	names := make([]string, 0, len(m.options.Summaries))
	for _, summary := range m.options.Summaries {
		names = append(names, summary.Name)
	}
	return names
}

// matchingCommands returns the command words matching the current input, for
// tab completion and the footer suggestion line.
func (m appModel) matchingCommands() []string {
	token := strings.Fields(m.shellInput)
	if len(token) > 1 {
		if token[0] == "dns" {
			return []string{"dns install", "dns remove", "dns edit"}
		}
		return shellCommands
	}
	prefix := strings.TrimSpace(m.shellInput)
	var matches []string
	for _, command := range shellCommands {
		if strings.HasPrefix(command, prefix) {
			matches = append(matches, command)
		}
	}
	for _, name := range m.connectionNames() {
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			matches = append(matches, name)
		}
	}
	if len(matches) == 0 {
		return shellCommands
	}
	return matches
}

// ParseShell turns a `:` command into an Outcome. current supplies the
// fallback connection for commands without an explicit name; names validates
// an explicit name (ignored when empty).
func ParseShell(input string, current string, names []string) (Outcome, error) {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) == 0 {
		return Outcome{}, fmt.Errorf("empty command")
	}
	command := fields[0]
	rest := fields[1:]
	known := false
	for _, candidate := range shellCommands {
		if candidate == command {
			known = true
			break
		}
	}
	if !known {
		for _, name := range names {
			if strings.EqualFold(name, command) && len(rest) == 0 {
				return Outcome{Action: ActOpen, Connection: name, View: ViewConnection}, nil
			}
		}
		return Outcome{}, fmt.Errorf("unknown command %q", command)
	}
	if command == "quit" || command == "exit" || command == "q" {
		return Outcome{Quit: true}, nil
	}
	if command == "new" {
		if len(rest) > 0 {
			return Outcome{}, fmt.Errorf("new takes no arguments")
		}
		return Outcome{Action: ActNew, View: ViewHome}, nil
	}
	if command == "edit" {
		return Outcome{Action: ActEdit, Connection: current, View: ViewConnection}, nil
	}
	if command == "help" {
		return Outcome{}, nil
	}

	if command == "dns" {
		if len(rest) == 0 {
			return Outcome{}, fmt.Errorf("dns needs a subcommand: dns install|remove|edit [connection]")
		}
		switch rest[0] {
		case "install":
			command = ActDNSSetup
		case "remove":
			command = ActDNSTeardown
		case "edit":
			command = ActDNSEdit
		default:
			return Outcome{}, fmt.Errorf("unknown dns subcommand %q (use install, remove, edit)", rest[0])
		}
		rest = rest[1:]
	}

	name := current
	if len(rest) > 1 {
		return Outcome{}, fmt.Errorf("too many arguments")
	}
	if len(rest) == 1 {
		name = rest[0]
	}
	if len(names) > 0 {
		found := false
		for _, candidate := range names {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			return Outcome{}, fmt.Errorf("unknown connection %q", name)
		}
	}
	if name == "" {
		return Outcome{}, fmt.Errorf("no connection specified — try: %s <name>", command)
	}
	return Outcome{Action: command, Connection: name, View: ViewHome}, nil
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func (m appModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	return m.chrome().View(m.body(), width, m.height)
}

func (m appModel) body() string {
	budget := m.chrome().Budget(m.height)
	if m.logSession != nil {
		query := m.searchQuery(searchLogs)
		lines := filterAndHighlightLines(m.logLines, query)
		if len(lines) == 0 && len(m.logLines) > 0 && query != "" {
			return "No log lines match " + query
		}
		return renderLogView(lines, m.logOffset, budget, m.logDone, m.logError)
	}
	var base string
	switch {
	case m.newWizard != nil:
		base = renderNewWizard(m.newWizard, budget, m.width, m.searchQuery(m.activeSearchScope()))
	case m.rescan != nil:
		base = renderRescanPicker(m.rescan, budget, m.width, m.searchQuery(searchRescan))
	case m.editor != nil:
		scope := searchEditor
		if len(m.editor.choices) > 0 {
			scope = searchEditorChoices
		}
		base = renderEditor(m.editor, budget, m.searchQuery(scope))
	case m.level() == 1:
		base = renderFilteredDetailCard(m.options.Detail, budget, m.searchQuery(searchDetail))
	case m.width == 0:
		// First frame, before the terminal size arrives.
		base = ""
	default:
		// Table photos: the table renders its own header line on top of
		// the height rows, so reserve one line for it.
		m.home.SetHeight(max(budget-1, 3))
		base = m.home.View()
	}
	if (m.help || m.actionModal != nil) && budget != maxInt {
		lines := strings.Count(base, "\n") + 1
		if lines < budget {
			base += strings.Repeat("\n", budget-lines)
		}
	}
	if m.help {
		return overlayCenter(base, helpPanel(m.width, budget), m.width)
	}
	if m.actionModal != nil {
		return overlayCenter(base, renderActionModal(m.actionModal, m.width, budget), m.width)
	}
	return base
}

func renderNewWizard(wizard *newConnectionWizard, budget, width int, query string) string {
	title := "NEW CONNECTION · " + strings.ToUpper(wizard.step)
	var rows []string
	var indices []int
	switch wizard.step {
	case "region":
		rows = wizard.start.Regions
		indices = matchingChoiceIndices(rows, query)
	case "kind":
		rows = newConnectionKinds
		indices = matchingChoiceIndices(rows, query)
	case "candidate":
		indices = matchingNewCandidates(wizard, query)
	case "routes":
		indices = matchingNewRoutes(wizard, query)
	}
	if budget <= 0 || budget == maxInt {
		budget = len(indices) + 3
	}
	rowBudget := max(budget-3, 1)
	position := indexOf(indices, wizard.cursor)
	if position < 0 && len(indices) > 0 {
		position = 0
	}
	start := max(position-rowBudget+1, 0)
	if lastStart := max(len(indices)-rowBudget, 0); start > lastStart {
		start = lastStart
	}
	end := min(start+rowBudget, len(indices))
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(title),
		chromeDimStyle.Render("↑/↓ select · enter continue · esc cancel · / search"),
		"",
	}
	if len(indices) == 0 {
		return strings.Join(append(lines, chromeDimStyle.Render("No matching options")), "\n")
	}
	for visible := start; visible < end; visible++ {
		index := indices[visible]
		marker := "  "
		var text string
		switch wizard.step {
		case "region":
			text = wizard.start.Regions[index]
		case "kind":
			text = newConnectionKinds[index]
		case "candidate":
			candidate := wizard.candidates[index]
			text = fmt.Sprintf("%s  %s  %s · %s", candidate.Name, candidate.InstanceID, candidate.Method, candidate.VpcID)
		case "routes":
			route := wizard.routes[index]
			selected := "[ ]"
			if route.Selected {
				selected = "[x]"
			}
			text = fmt.Sprintf("%s %s · %s · %s", selected, route.CIDR, route.Source, route.Description)
		}
		if index == wizard.cursor {
			marker = "▶ "
		}
		row := truncate(marker+text, max(width-2, 1))
		if index == wizard.cursor {
			row = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("10")).Render(row)
		} else {
			row = highlightText(row, query)
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

func renderRescanPicker(selector *rescanSelector, budget, width int, query string) string {
	if budget <= 0 || budget == maxInt {
		budget = len(selector.candidates) + 3
	}
	indices := matchingRescanCandidates(selector, query)
	rowBudget := max(budget-3, 1)
	position := indexOf(indices, selector.cursor)
	if position < 0 && len(indices) > 0 {
		position = 0
	}
	start := max(position-rowBudget+1, 0)
	if lastStart := max(len(indices)-rowBudget, 0); start > lastStart {
		start = lastStart
	}
	end := min(start+rowBudget, len(indices))
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("SELECT NEW PIVOT · " + selector.connection),
		chromeDimStyle.Render("The current pivot is marked · enter save · esc cancel"),
		"",
	}
	if len(indices) == 0 {
		return strings.Join(append(lines, chromeDimStyle.Render("No matching pivots")), "\n")
	}
	for position := start; position < end; position++ {
		index := indices[position]
		candidate := selector.candidates[index]
		name := candidate.Name
		if name == "" {
			name = "unnamed"
		}
		marker := "  "
		if index == selector.cursor {
			marker = "▶ "
		}
		current := ""
		if candidate.Current {
			current = " [current]"
		}
		egress := "egress limited"
		if candidate.EgressOK {
			egress = "egress ok"
		}
		row := fmt.Sprintf("%s%s  %s  %s · %s", marker, name+current, candidate.InstanceID, candidate.Method, egress)
		row = truncate(row, max(width-2, 1))
		if index == selector.cursor {
			row = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("10")).Render(row)
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

func renderFilteredDetailCard(detail *ConnDetail, budget int, query string) string {
	lines := filterAndHighlightLines(detailCardLines(detail), query)
	if strings.TrimSpace(query) != "" && len(lines) == 0 {
		return "No matching connection details"
	}
	if budget > 0 && len(lines) > budget {
		lines = append(lines[:max(budget-1, 0)], chromeDimStyle.Render("… more matches below"))
	}
	return strings.Join(lines, "\n")
}

func filterAndHighlightLines(lines []string, query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return lines
	}
	var matches []string
	lowerQuery := strings.ToLower(query)
	for _, line := range lines {
		plain := ansiEscapePattern.ReplaceAllString(line, "")
		if strings.Contains(strings.ToLower(plain), lowerQuery) {
			matches = append(matches, highlightText(plain, query))
		}
	}
	return matches
}

func highlightText(text, query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return text
	}
	matcher := regexp.MustCompile("(?i)" + regexp.QuoteMeta(query))
	style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11"))
	return matcher.ReplaceAllStringFunc(text, func(match string) string { return style.Render(match) })
}

func renderEditor(editor *connectionEditor, budget int, query string) string {
	if editor.routePicker {
		indices := matchingEditorRoutes(editor.routes, query)
		if budget <= 0 || budget == maxInt {
			budget = len(indices) + 3
		}
		lines := []string{
			lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("SELECT ROUTES · " + editor.originalName),
			chromeDimStyle.Render("↑/↓ move · space toggle · a all · m add CIDR · enter apply · esc discard"),
			"",
		}
		if editor.addingRoute {
			lines = append(lines, "Additional CIDR: "+editor.routeInput+"▏")
			if editor.routeError != "" {
				lines = append(lines, chromeErrStyle.Render(editor.routeError))
			}
			return strings.Join(lines, "\n")
		}
		if len(indices) == 0 {
			return strings.Join(append(lines, chromeDimStyle.Render("No matching routes")), "\n")
		}
		rows := max(budget-len(lines), 1)
		position := indexOf(indices, editor.routeCursor)
		if position < 0 {
			position = 0
		}
		start := max(position-rows+1, 0)
		if lastStart := max(len(indices)-rows, 0); start > lastStart {
			start = lastStart
		}
		end := min(start+rows, len(indices))
		for visible := start; visible < end; visible++ {
			index := indices[visible]
			route := editor.routes[index]
			checked := "[ ]"
			if route.Selected {
				checked = "[x]"
			}
			row := fmt.Sprintf("%s %s · %s · %s", checked, route.CIDR, route.Source, route.Description)
			if index == editor.routeCursor {
				row = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("10")).Render("▶ " + highlightText(row, query))
			} else {
				row = highlightText(row, query)
			}
			lines = append(lines, row)
		}
		return strings.Join(lines, "\n")
	}
	if len(editor.choices) > 0 {
		lines := []string{
			lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("SELECT " + strings.ToUpper(editor.fields[editor.field])),
			chromeDimStyle.Render("↑/↓ move · enter select · esc cancel"),
			"",
		}
		if budget <= 0 || budget == maxInt {
			budget = len(editor.choices) + len(lines)
		}
		indices := matchingChoiceIndices(editor.choices, query)
		if len(indices) == 0 {
			return strings.Join(append(lines, chromeDimStyle.Render("No matching options")), "\n")
		}
		for _, index := range indices {
			if len(lines) >= budget {
				break
			}
			choice := editor.choices[index]
			row := "  " + choice
			if index == editor.choiceCursor {
				row = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("10")).Render("▶ " + choice)
			}
			lines = append(lines, row)
		}
		return strings.Join(lines, "\n")
	}
	if budget <= 0 || budget == maxInt {
		budget = len(editor.fields) + 4
	}
	indices := matchingEditorFields(editor, query)
	if len(indices) == 0 {
		return "No matching fields"
	}
	activePosition := indexOf(indices, editor.field)
	if activePosition < 0 {
		activePosition = 0
	}
	fieldRows := max(budget-4, 1)
	start := max(activePosition-fieldRows+1, 0)
	if lastStart := max(len(indices)-fieldRows, 0); start > lastStart {
		start = lastStart
	}
	end := min(start+fieldRows, len(indices))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	active := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("10"))
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("EDIT CONNECTION · " + editor.originalName),
		muted.Render(fmt.Sprintf("Field %d/%d · %s", editor.field+1, len(editor.fields), editor.fields[editor.field])),
		muted.Render("↑/↓ or tab move · enter options/next · ctrl+s save · esc cancel"),
		"",
	}
	if editor.conflicted {
		lines[1] = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).Render("Changed on disk · Esc to cancel and reopen")
	}
	for position := start; position < end; position++ {
		i := indices[position]
		text := fmt.Sprintf("%-16s %s", editor.fields[i], editor.values[i])
		if i == editor.field {
			text += "▏"
			lines = append(lines, active.Render("▶ "+highlightText(text, query)))
		} else {
			lines = append(lines, "  "+highlightText(text, query))
		}
	}
	return strings.Join(lines, "\n")
}

func matchingChoiceIndices(choices []string, query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	indices := make([]int, 0, len(choices))
	for index, choice := range choices {
		if query == "" || strings.Contains(strings.ToLower(choice), query) {
			indices = append(indices, index)
		}
	}
	return indices
}

func matchingEditorFields(editor *connectionEditor, query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	indices := make([]int, 0, len(editor.fields))
	for index, field := range editor.fields {
		if query == "" || strings.Contains(strings.ToLower(field+" "+editor.values[index]), query) {
			indices = append(indices, index)
		}
	}
	return indices
}

func matchingEditorRoutes(routes []config.NetworkRoute, query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	indices := make([]int, 0, len(routes))
	for index, route := range routes {
		searchable := strings.ToLower(strings.Join([]string{route.CIDR, route.Source, route.TargetType, route.TargetID, route.Description}, " "))
		if query == "" || strings.Contains(searchable, query) {
			indices = append(indices, index)
		}
	}
	return indices
}

func matchingRescanCandidates(selector *rescanSelector, query string) []int {
	query = strings.ToLower(strings.TrimSpace(query))
	indices := make([]int, 0, len(selector.candidates))
	for index, candidate := range selector.candidates {
		searchable := strings.ToLower(strings.Join([]string{candidate.Name, candidate.InstanceID, candidate.Method, candidate.Target, candidate.VpcID, strings.Join(candidate.VpcCIDRs, " ")}, " "))
		if query == "" || strings.Contains(searchable, query) {
			indices = append(indices, index)
		}
	}
	return indices
}

func indexOf(indices []int, target int) int {
	for position, index := range indices {
		if index == target {
			return position
		}
	}
	return -1
}

func neighboringEditorField(editor *connectionEditor, query string, direction int) int {
	indices := matchingEditorFields(editor, query)
	if len(indices) == 0 {
		return editor.field
	}
	position := indexOf(indices, editor.field)
	if position < 0 {
		if direction < 0 {
			return indices[len(indices)-1]
		}
		return indices[0]
	}
	position = min(max(position+direction, 0), len(indices)-1)
	return indices[position]
}

func renderLogView(lines []string, offset, budget int, done bool, err error) string {
	if len(lines) == 0 {
		if err != nil {
			return "logs: " + err.Error()
		}
		return "starting log stream..."
	}
	end := len(lines) - offset
	start := max(end-budget, 0)
	if offset > 0 {
		start = max(end-budget, 0)
	}
	visible := append([]string(nil), lines[start:end]...)
	if done {
		visible = append(visible, "", "log stream ended")
	}
	if err != nil {
		visible = append(visible, "error: "+err.Error())
	}
	if len(visible) > budget {
		visible = visible[len(visible)-budget:]
	}
	return strings.Join(visible, "\n")
}

func renderActionModal(modal *actionModal, width, height int) string {
	innerWidth := min(max(width-8, 24), 78)
	lines := []string{lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(modal.title)}
	for _, line := range strings.Split(strings.TrimSpace(modal.body), "\n") {
		lines = append(lines, truncate(line, innerWidth))
	}
	if !modal.running {
		lines = append(lines, "", chromeDimStyle.Render("enter/esc close"))
	}
	if height > 2 && len(lines) > height-2 {
		lines = lines[:height-2]
	}
	border := helpBorderStyle
	if modal.failed {
		border = border.BorderForeground(lipgloss.Color("1"))
	}
	return border.Render(strings.Join(lines, "\n"))
}

// helpPanel renders the `?` window content inside a rounded border,
// capped to the body budget.
func helpPanel(width, height int) string {
	lines := helpPanelLines(width)
	// Account for the two border rows.
	if height > 2 && len(lines) > height-2 {
		lines = lines[:height-2]
	}
	return helpBorderStyle.Render(strings.Join(lines, "\n"))
}

// helpPanelLines assembles the navigation / action / shell reference rows
// shown in the overlaid help window.
func helpPanelLines(width int) []string {
	inner := width - 6
	switch {
	case inner > 78:
		inner = 78
	case inner < 24:
		inner = 24
	}
	section := func(text string) string {
		return truncate(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(text), inner)
	}
	row := func(key, text string) string {
		return truncate(chromeKeyStyle.Render(fmt.Sprintf("%-20s", key))+text, inner)
	}
	return []string{
		section("Navigation"),
		row("↑/↓ · pgup/pgdn", "move · page the selection"),
		row("enter", "open the highlighted connection"),
		row("esc", "back (quits from the home screen)"),
		row("/", "filter connections · enter apply · esc cancel"),
		row(":", "commands or :<connection> shortcut · tab completes"),
		row("?", "toggle this help · q quits"),
		section("Actions — home"),
		row("n", "new connection (needs AWS)"),
		row("s · x", "start · stop the tunnel"),
		row("l · i", "live logs · status"),
		row("d · e · v", "DNS · edit connection · check AWS"),
		row("r · f", "rescan · forget"),
		row("a", "launch the tunnel in the foreground"),
		section("Actions — connection view"),
		row("p", "print the exact sshuttle command"),
		row("1 · 2", "install · remove the split-DNS resolver"),
		row("d · e · v", "DNS · edit connection · check AWS"),
		section("Shell commands (:)"),
		row("start|stop|…", "start, stop, launch, logs, status, print,"),
		row("", "\trescan, forget — optional [connection]"),
		row("dns …", "dns install|remove|edit [connection]"),
		row("n/h/q", "new · help · quit"),
		section("Status dots"),
		row("● ○", "green running · gray idle · red unreachable"),
		row("◐", "yellow: running, split-DNS resolver missing"),
	}
}

func (m appModel) chrome() Chrome {
	var chrome Chrome
	if m.level() == 1 {
		chrome = m.connectionChrome()
	} else {
		chrome = m.homeChrome()
	}
	if m.loadingDetail != "" {
		chrome.Info = "loading " + m.loadingDetail + "…"
	}
	if m.editor != nil {
		if m.editor.routePicker {
			chrome.Hints = []Hint{{"↑/↓", "move"}, {"space", "toggle"}, {"a", "all"}, {"m", "add CIDR"}, {"enter", "apply"}, {"/", "search"}, {"esc", "discard"}}
		} else if len(m.editor.choices) > 0 {
			chrome.Hints = []Hint{{"↑/↓", "choose"}, {"enter", "select"}, {"esc", "cancel"}}
		} else {
			chrome.Hints = []Hint{{"↑/↓", "move"}, {"tab", "next"}, {"enter", "options"}, {"ctrl+s", "save"}, {"v", "check access"}, {"esc", "cancel"}}
		}
	}
	if m.newWizard != nil {
		chrome.Title = "bastionctl · new connection"
		chrome.Info = m.newWizard.step
		chrome.Hints = []Hint{{"↑/↓", "select"}, {"enter", "continue"}, {"/", "search"}, {"esc", "back"}}
		if m.newWizard.step == "routes" {
			chrome.Hints = []Hint{{"↑/↓", "move"}, {"space", "toggle"}, {"a", "all"}, {"enter", "continue"}, {"/", "search"}, {"esc", "back"}}
		}
	}
	if m.rescan != nil {
		chrome.Title = "bastionctl · rescan · " + m.rescan.connection
		chrome.Info = fmt.Sprintf("%d/%d candidates", m.rescan.cursor+1, len(m.rescan.candidates))
		chrome.Hints = []Hint{{"↑/↓", "select pivot"}, {"enter", "save"}, {"esc", "cancel"}}
	}
	if m.logSession != nil {
		chrome.Title = "bastionctl · " + m.logTitle
		chrome.Info = "streaming"
		if m.logDone {
			chrome.Info = "ended"
		}
		chrome.Hints = []Hint{{"esc", "close"}, {"q", "close"}, {"↑/↓", "scroll"}, {"pgup/pgdn", "page"}}
	}
	if m.options.SudoCheck != nil && m.sudo == sudoMissing {
		if chrome.Info != "" {
			chrome.Info += " · "
		}
		chrome.Info += Dot("warn") + " sudo needed"
	}
	if m.searching {
		chrome.SearchOpen = true
		chrome.Search = m.searchInput
	}
	return chrome
}

func (m appModel) homeChrome() Chrome {
	summaries := m.visibleSummaries()
	count := len(summaries)
	running := 0
	for _, summary := range summaries {
		if summary.Running {
			running++
		}
	}
	info := fmt.Sprintf("%d connections · %d running", count, running)
	if count == 0 {
		info = "no connections"
	}
	if m.options.Offline {
		info = Dot("warn") + " offline · " + info + " (cached)"
	}
	hints := []Hint{
		{"enter", "open"},
		{"n", "new"},
		{"s", "start"},
		{"x", "stop"},
		{"a", "launch"},
		{"l", "logs"},
		{"i", "status"},
		{"d", "dns"},
		{"e", "edit"},
		{"v", "check AWS"},
		{"/", "search"},
		{"r", "rescan"},
		{"f", "forget"},
		{":", "cmd"},
		{"?", "help"},
		{"q", "quit"},
	}
	title := "bastionctl · connections"
	if m.options.Account != "" {
		title += " · " + m.options.Account
	}
	if m.options.Profile != "" {
		title += " · profile " + m.options.Profile
	}
	chrome := Chrome{Title: title, Info: info, Hints: hints, Error: m.errorMsg}
	if m.searching {
		chrome.SearchOpen = true
		chrome.Search = m.searchInput
	}
	if m.shell {
		chrome.ShellOpen = true
		chrome.Shell = m.shellInput
		chrome.ShellSuggestions = m.matchingCommands()
	}
	return chrome
}

func (m appModel) connectionChrome() Chrome {
	detail := m.options.Detail
	state := "ok"
	if !detail.Running {
		if detail.TargetOK {
			state = "idle"
		} else {
			state = "fail"
		}
	}
	info := Dot(state) + " " + connectionStateLabel(detail)
	if m.options.Offline {
		info = Dot("warn") + " offline · " + info
	}
	if detail.DNSSummary != "" {
		info += " · " + detail.DNSSummary
	}
	hints := []Hint{
		{"esc", "back"},
		{"s", "start"},
		{"x", "stop"},
		{"l", "logs"},
		{"i", "status"},
		{"p", "print"},
		{"e", "edit"},
		{"d", "dns"},
		{"v", "check AWS"},
		{"r", "rescan"},
		{"f", "forget"},
		{":", "cmd"},
		{"?", "help"},
		{"q", "quit"},
	}
	if detail.DNSWantsInstall {
		// Insert the resolver install/remove hints before the closing ones.
		last := hints[len(hints)-1]
		hints = append(hints[:len(hints)-1], Hint{"1", "install"}, Hint{"2", "remove"}, Hint{"?", "help"}, last)
	}
	title := fmt.Sprintf("bastionctl · %s · %s/%s · %s", detail.Name, detail.Kind, detail.Method, detail.Region)
	chrome := Chrome{Title: title, Info: info, Hints: hints, Error: m.errorMsg}
	if m.shell {
		chrome.ShellOpen = true
		chrome.Shell = m.shellInput
		chrome.ShellSuggestions = m.matchingCommands()
	}
	return chrome
}

func connectionStateLabel(detail *ConnDetail) string {
	if detail.Running {
		return fmt.Sprintf("running (pid %d)", detail.PID)
	}
	if !detail.TargetOK {
		return "target unreachable"
	}
	return "idle"
}

// renderDetailCard draws the connection view body from the k9s-describe
// style section list, trimmed to the body budget.
func renderDetailCard(detail *ConnDetail, budget int) string {
	lines := detailCardLines(detail)
	if budget > 0 && len(lines) > budget {
		lines = lines[:max(budget-1, 0)]
		lines = append(lines, chromeDimStyle.Render("… (more below)"))
	}
	return strings.Join(lines, "\n")
}

// detailCardLines draws the connection view body in the k9s "describe"
// style: a fixed label column and always the same section order
// (TUNNEL/TARGET/DNS/ROUTES/COMMAND), one fact per line, so the form is
// stable regardless of the connection's state. Optional sub-lines
// (resolver state, route list) indent under their section.
func detailCardLines(detail *ConnDetail) []string {
	var lines []string
	add := func(label string, text string) {
		key := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(fmt.Sprintf("%-9s", label+":"))
		lines = append(lines, key+text)
	}
	addRaw := func(text string) { lines = append(lines, text) }

	// TUNNEL
	tunnel := Dot("ok") + fmt.Sprintf(" running · PID %d", detail.PID) + chromeDimStyle.Render("  ·  logs: bastionctl --logs")
	if !detail.Running {
		tunnel = Dot("idle") + " idle" + chromeDimStyle.Render("  ·  start with s")
	}
	add("TUNNEL", tunnel)

	// TARGET
	awsProfile := detail.AWSProfile
	if awsProfile == "" {
		awsProfile = "default"
	}
	add("AWS", awsProfile)

	// TARGET
	target := detail.Target
	probe := chromeDimStyle.Render(fmt.Sprintf(" (%s, port %d)", detail.Method, detail.SSHPort))
	if detail.TargetOK {
		target += " " + Dot("ok") + chromeDimStyle.Render(" reachable"+probe)
	} else {
		target += " " + Dot("fail") + chromeDimStyle.Render(" unreachable"+probe)
	}
	if detail.ReachDetail != "" {
		target += chromeDimStyle.Render(" · " + strings.ReplaceAll(strings.TrimSpace(detail.ReachDetail), "\n", " "))
	}
	add("TARGET", target)

	// DNS — the section is always present so the form holds; optional
	// domain/resolver facts indent below like k9s describe blocks.
	dns := detail.DNSSummary
	if dns == "" {
		dns = "off"
	} else if detail.DNSWantsInstall {
		dns += chromeDimStyle.Render(" · resolver config missing — run :dns install (or press 1)")
	} else if detail.DNSInstalled != "" {
		dns += chromeDimStyle.Render(" · " + detail.DNSInstalled)
	}
	add("DNS", dns)
	if len(detail.Domains) > 0 {
		addRaw("  " + chromeDimStyle.Render("domains: "+strings.Join(detail.Domains, ", ")))
	}
	if len(detail.Resolver) > 0 {
		addRaw("  " + chromeDimStyle.Render("resolver: "+strings.Join(detail.Resolver, ", ")))
	}

	// ROUTES — header row plus one line per route (k9s node/pod lists).
	add("ROUTES", fmt.Sprintf("%d of %d selected", detail.SelectedCount(), len(detail.Routes)))
	for _, route := range detail.Routes {
		marker := Dot("ok")
		text := route.CIDR
		if route.Source != "" {
			text += chromeDimStyle.Render(" · " + route.Source)
		}
		if route.TargetID != "" {
			text += chromeDimStyle.Render(" · " + route.TargetID)
		} else if route.Description != "" {
			text += chromeDimStyle.Render(" · " + route.Description)
		}
		if route.Selected {
			text = chromeDimStyle.Render("  " + marker + " " + text)
		} else {
			text = chromeDimStyle.Render("  " + Dot("idle") + " " + text)
		}
		addRaw(text)
	}
	if len(detail.Routes) == 0 {
		addRaw(chromeDimStyle.Render("  ○ none · run :rescan to pick networks"))
	}

	// COMMAND
	command := detail.Command
	if command == "" {
		command = chromeDimStyle.Render("(sshuttle not on PATH — fix and press p)")
	}
	add("COMMAND", command)

	return lines
}

func (detail *ConnDetail) SelectedCount() int {
	count := 0
	for _, route := range detail.Routes {
		if route.Selected {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// Home table
// ---------------------------------------------------------------------------

// homeColSpec describes one table column: its title, cell extraction,
// display importance (rank 0 = always shown), and width bounds. Columns
// join in importance order while they fit, and the last visible column
// absorbs leftover width so rows span the full terminal (k9s style).
type homeColSpec struct {
	title   string
	cellOf  func(ConnSummary) string
	minW    int // minimum display width incl. gap
	capW    int // maximum display width incl. gap
	rank    int // importance: lower joins first
	width   int // fitted display width
	include bool
}

// homeLayout fits the connection table to its content and the terminal
// width. It returns the visible columns and rows.
func homeLayout(summaries []ConnSummary, width int) ([]table.Column, []table.Row) {
	specs := []homeColSpec{
		// Display order: essentials first, then metadata.
		{title: "Status", cellOf: func(s ConnSummary) string { return s.StatusLabel() }, minW: 12, capW: 34, rank: 0},
		{title: "Connection", cellOf: func(s ConnSummary) string { return s.Name }, minW: 12, capW: 28, rank: 0},
		{title: "Region", cellOf: (func(s ConnSummary) string { return s.Region }), minW: 8, capW: 15, rank: 1},
		{title: "AWS Profile", cellOf: func(s ConnSummary) string { return s.AWSProfileLabel() }, minW: 12, capW: 36, rank: 2},
		{title: "DNS", cellOf: func(s ConnSummary) string { return s.DNS }, minW: 12, capW: 36, rank: 3},
		{title: "Networks", cellOf: func(s ConnSummary) string { return s.Networks }, minW: 16, capW: 50, rank: 4},
		{title: "Account", cellOf: func(s ConnSummary) string { return s.Account }, minW: 8, capW: 15, rank: 5},
		{title: "Kind", cellOf: func(s ConnSummary) string { return s.Kind }, minW: 5, capW: 11, rank: 6},
		{title: "Method", cellOf: func(s ConnSummary) string { return s.Method }, minW: 5, capW: 9, rank: 7},
	}

	// Fit each column to the longest of its title and cells (+2 cells of
	// gap), bounded by min/max.
	for i := range specs {
		w := lipgloss.Width(specs[i].title) + 2
		for _, summary := range summaries {
			cell := specs[i].cellOf(summary)
			if cellW := lipgloss.Width(cell) + 2; cellW > w {
				w = cellW
			}
		}
		specs[i].width = min(max(w, specs[i].minW), specs[i].capW)
	}

	// Essentials always render; the rest join while they fit.
	used := 0
	for i := range specs {
		if specs[i].rank == 0 {
			specs[i].include = true
			used += specs[i].width
		}
	}
	for rank := 1; rank <= 6; rank++ {
		for i := range specs {
			if specs[i].rank == rank && used+specs[i].width <= width {
				specs[i].include = true
				used += specs[i].width
				break
			}
		}
	}
	if used > width {
		// Tiny terminal: even the essentials overflow; let bubbles
		// truncate the rendered rows naturally.
		for i := range specs {
			specs[i].width = min(specs[i].width, max(width/2, specs[i].minW))
		}
	}

	// The last included column soaks up the remainder so the selection
	// bar and rules span the full terminal width.
	last := -1
	for i := range specs {
		if specs[i].include {
			last = i
		}
	}
	if last >= 0 {
		specs[last].width += max(width-used, 0)
	}

	var columns []table.Column
	for _, spec := range specs {
		if !spec.include {
			continue
		}
		columns = append(columns, table.Column{Title: spec.title, Width: max(spec.width-2, 1)})
	}
	rows := make([]table.Row, 0, len(summaries))
	for _, summary := range summaries {
		var row table.Row
		for _, spec := range specs {
			if spec.include {
				row = append(row, spec.cellOf(summary))
			}
		}
		rows = append(rows, row)
	}
	return columns, rows
}

func newHomeTable(summaries []ConnSummary, highlight int) table.Model {
	columns, rows := homeLayout(summaries, 120)
	tableModel := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(10),
		table.WithStyles(sharedTableStyles()),
	)
	if highlight > 0 && highlight < len(summaries) {
		tableModel.SetCursor(highlight)
	}
	return tableModel
}
