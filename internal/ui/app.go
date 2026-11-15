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
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"

	"bastionctl/internal/config"
	"bastionctl/internal/ui/table"
	tea "bastionctl/internal/ui/tea"
	"bastionctl/internal/ui/textinput"
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

// StatusLabel renders a plain-text home-table status cell. The table widget
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
	Account           string // header context (account id/alias)
	AccountLabels     map[string]string
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
	// SudoAuthenticate validates a password entered in the in-app prompt.
	SudoCheck        func() error
	SudoAuthenticate func(string) error
	// CaptureSender, when set, receives the live program's Send hook right
	// before RunApp blocks. Background action handlers then surface
	// in-app prompts (e.g. the sudo password field) into the UI:
	// SudoPasswordRequestMsg shows the modal, and the reply channel gets
	// the password ("" when the user cancelled). The hook must stop being
	// used after RunApp returns.
	CaptureSender func(func(Message))

	// Version is the build version shown on the home dashboard. Empty
	// falls back to "dev".
	Version string
}

// Message is the value type exchanged with the interactive browser. It is an
// alias for the runtime's message type so callers do not need to import it.
type Message = tea.Msg

// RunApp opens the interactive two-level browser and returns its outcome.
func RunApp(options RunOptions) (Outcome, error) {
	model := appModel{
		options:      options,
		knownConfigs: options.ConfigSnapshot,
		home:         newHomeTable(options.Summaries, options.Highlight),
		errorMsg:     options.Error,
		errorTTL:     noticeTTLSeconds,
		outcome:      Outcome{View: ViewHome},
	}
	if options.Level == 1 {
		if options.Detail == nil {
			return Outcome{Quit: true}, fmt.Errorf("connection view requested without detail data")
		}
		model.outcome.Connection = options.Detail.Name
		model.outcome.View = ViewConnection
	}
	program := tea.NewProgram(model, tea.WithAltScreen())
	clearCaptureSender := registerCaptureSender(options.CaptureSender, program.Send)
	defer clearCaptureSender()
	result, err := program.Run()
	if err != nil {
		return Outcome{Quit: true}, err
	}
	return result.(appModel).outcome, nil
}

func registerCaptureSender(capture func(func(Message)), send func(Message)) func() {
	if capture == nil {
		return func() {}
	}
	capture(send)
	return func() { capture(nil) }
}

// ---------------------------------------------------------------------------
// Sudo authorization modal
// ---------------------------------------------------------------------------

// SudoPasswordRequestMsg asks the running UI to collect a sudo password
// through an in-app modal. Background action handlers send it through the
// Send hook delivered by RunOptions.CaptureSender. Reply (which must be
// buffered, size 1) receives the typed password, or "" on cancel.
type SudoPasswordRequestMsg struct {
	Reason string        // context line shown above the input
	Reply  chan<- string // receives the password, or "" on cancel
}

// sudoPasswordPrompt is the modal state while a SudoPasswordRequestMsg is
// active: a masked text input plus the reply channel to answer on.
type sudoPasswordPrompt struct {
	request      SudoPasswordRequestMsg
	input        textinput.Model
	authenticate bool
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type appModel struct {
	options       RunOptions
	knownConfigs  []*config.Config
	width         int
	height        int
	home          table.Model
	searching     bool
	searchInput   string
	searchFilter  string
	searchFilters map[string]string
	searchTarget  string
	shell         bool // the `:` command line is open
	shellInput    string
	help          bool   // the `?` navigation help overlay is shown
	errorMsg      string // transient notice shown in the footer
	errorTTL      int    // seconds left before errorMsg clears itself
	loadingDetail string
	actionID      int
	actionModal   *actionModal
	logSession    *LogSession
	logLines      []string
	logOffset     int
	logDone       bool
	logError      error
	logTitle      string
	editor        *connectionEditor
	rescan        *rescanSelector
	newWizard     *newConnectionWizard
	sudo          sudoState
	sudoPending   *sudoRequest
	sudoPrompt    *sudoPasswordPrompt
	outcome       Outcome
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
type noticeTickMsg struct{}
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
	cursor        int // rune offset of the caret inside input
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

	// yamlMode is the inline YAML editor: yamlLines is the draft document,
	// yamlRow/yamlCol the caret, and yamlOffset the first visible line.
	yamlMode       bool
	yamlLines      []string
	yamlRow        int
	yamlCol        int
	yamlOffset     int
	yamlError      string
	vimInsert      bool
	vimPending     bool
	vimUndo        []editorSnapshot
	vimPrompt      string
	vimInput       string
	vimSaving      bool
	discardConfirm bool
	discardTarget  string
	savedValues    []string
	savedRoutes    []config.NetworkRoute
	yamlBaseline   string
}

type connectionRefreshedMsg struct {
	id      int
	name    string
	summary ConnSummary
	detail  *ConnDetail
	err     error
}

// editorCursorEnd returns the caret offset just past s, where the caret sits
// when a field gains focus.
func editorCursorEnd(s string) int { return utf8.RuneCountInString(s) }

// insertAtRune inserts text at a rune offset.
func insertAtRune(s string, offset int, insert string) string {
	runes := []rune(s)
	if offset < 0 {
		offset = 0
	}
	if offset > len(runes) {
		offset = len(runes)
	}
	out := make([]rune, 0, len(runes)+len([]rune(insert)))
	out = append(out, runes[:offset]...)
	out = append(out, []rune(insert)...)
	out = append(out, runes[offset:]...)
	return string(out)
}

// deleteRuneBefore removes the rune just before a rune offset.
func deleteRuneBefore(s string, offset int) string {
	runes := []rune(s)
	if offset <= 0 || offset > len(runes) {
		return s
	}
	return string(append(runes[:offset-1], runes[offset:]...))
}

// deleteRuneAt removes the rune at a rune offset.
func deleteRuneAt(s string, offset int) string {
	runes := []rune(s)
	if offset < 0 || offset >= len(runes) {
		return s
	}
	return string(append(runes[:offset], runes[offset+1:]...))
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
	cmds = append(cmds, m.nextNoticeTick())
	return tea.Batch(cmds...)
}

// noticeTTLSeconds is how long a transient notice stays in the footer before
// it clears itself. The footer shows the remaining seconds next to the text.
const noticeTTLSeconds = 5

// setError publishes a transient notice in the footer and (re)starts its
// countdown.
func (m *appModel) setError(text string) {
	m.errorMsg = text
	m.errorTTL = noticeTTLSeconds
}

// clearError dismisses the current notice immediately.
func (m *appModel) clearError() {
	m.errorMsg = ""
	m.errorTTL = 0
}

// notice renders the footer notice with its remaining lifetime, e.g.
// "4s · open corp: timeout". The countdown leads so that a long message
// truncated on a narrow terminal never hides it. It is empty when there is
// no notice.
func (m appModel) notice() string {
	if m.errorMsg == "" {
		return ""
	}
	ttl := m.errorTTL
	if ttl < 1 {
		ttl = 1
	}
	return fmt.Sprintf("%ds · %s", ttl, m.errorMsg)
}

func (m appModel) nextNoticeTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return noticeTickMsg{} })
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
	case noticeTickMsg:
		if m.errorMsg != "" {
			if m.errorTTL <= 0 {
				// A notice set without an explicit TTL starts its
				// countdown now rather than vanishing immediately.
				m.errorTTL = noticeTTLSeconds
			} else {
				m.errorTTL--
			}
			if m.errorTTL <= 0 {
				m.clearError()
			}
		}
		return m, m.nextNoticeTick()
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
				m.setError("config refresh: " + message.err.Error())
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
			m.setError("edit: " + message.err.Error())
			return m, nil
		}
		m.editor = newConnectionEditor(message.cfg, m.options.AWSProfiles, m.options.AWSRegions)
		m.editor.field = min(message.startField, len(m.editor.fields)-1)
		m.editor.input = m.editor.values[m.editor.field]
		m.editor.cursor = 0
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
	case vimSaveFinishedMsg:
		return m.finishVimSave(message)
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
				m.setError("connection refresh failed: " + message.err.Error())
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
	case SudoPasswordRequestMsg:
		input := textinput.New()
		input.EchoMode = textinput.EchoPassword
		input.EchoCharacter = '•'
		input.Focus()
		m.sudoPrompt = &sudoPasswordPrompt{request: message, input: input}
		return m, textinput.Blink
	case detailLoadedMsg:
		if message.name != m.loadingDetail {
			return m, nil
		}
		m.loadingDetail = ""
		if message.err != nil {
			m.setError("open " + message.name + ": " + message.err.Error())
			return m, nil
		}
		if message.detail == nil {
			m.setError("open " + message.name + ": connection is no longer available")
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
		m.home.SetWidth(max(m.homeTableWidth(), 40))
		budget := m.chrome().Budget(m.height, m.width)
		// The table sits inside the bordered panel, which adds two rows.
		m.home.SetHeight(max(budget-2, 2))
		m.refreshHomeTable(false)
		return m, nil
	case tea.KeyMsg:
		key := message.String()
		if m.sudoPrompt != nil {
			return m.updateSudoPassword(key, message)
		}
		if m.editor != nil && m.editor.vimSaving {
			return m, nil
		}
		if m.editor != nil && m.editor.discardConfirm {
			switch key {
			case "y":
				m.editor.discardConfirm = false
				switch m.editor.discardTarget {
				case "route":
					m.editor.routes = m.editor.routeOriginal
					m.editor.routePicker = false
				case "route-input":
					m.editor.addingRoute = false
					m.editor.routeInput = ""
					m.editor.routeError = ""
				default:
					m.closeVimEditor()
				}
				if m.editor != nil {
					m.editor.discardTarget = ""
				}
			case "n", "esc":
				m.editor.discardConfirm = false
				m.editor.discardTarget = ""
			}
			return m, nil
		}
		if m.editor != nil && m.editor.vimPrompt != "" {
			return m.updateVimPrompt(key, message.Runes)
		}
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
						input := textinput.New()
						input.EchoMode = textinput.EchoPassword
						input.EchoCharacter = '•'
						input.Focus()
						m.sudoPrompt = &sudoPasswordPrompt{input: input, authenticate: true}
						return m, textinput.Blink
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
						m.setError("action is unavailable")
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
			m.clearError()
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
		if key == "esc" && m.editor == nil {
			scope := m.activeSearchScope()
			if m.searchQuery(scope) != "" {
				m.clearSearch(scope)
				return m, nil
			}
		}
		if m.editor != nil && key == "esc" {
			editor := m.editor
			switch {
			case editor.routePicker && editor.addingRoute && editor.routeInput != "":
				editor.discardConfirm, editor.discardTarget = true, "route-input"
				return m, nil
			case editor.routePicker && !editor.addingRoute && !reflect.DeepEqual(editor.routes, editor.routeOriginal):
				editor.discardConfirm, editor.discardTarget = true, "route"
				return m, nil
			case !editor.routePicker && len(editor.choices) == 0:
				if editor.isDirty() {
					editor.discardConfirm = true
				} else {
					m.closeVimEditor()
				}
				return m, nil
			}
		}
		if m.editor != nil && !m.editor.vimInsert && !m.editor.addingRoute && key == "?" {
			m.help = true
			return m, nil
		}
		// The inline YAML editor owns every key, including "/" (needed for
		// CIDRs and paths) and q/?/: which are ordinary YAML characters there.
		if m.editor != nil && (m.editor.yamlMode || m.editor.vimInsert) {
			return m.updateEditor(key, message.Runes)
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
			// At home there is nothing to go back to. Escape is a
			// navigation key only; quitting is q / ctrl+c.
			return m, nil
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
	visible := m.visibleSummaries()
	cursor := m.home.Cursor()
	hadSelection := cursor >= 0 && cursor < len(visible)
	var selected ConnSummary
	if hadSelection {
		selected = visible[cursor]
	}
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
				m.setError("connection changed on disk; cancel and reopen edit before saving")
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
	if hadSelection {
		for index, summary := range m.visibleSummaries() {
			if configKey(summary.Account, summary.Name) == configKey(selected.Account, selected.Name) {
				m.home.SetCursor(index)
				break
			}
		}
	}

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
		m.setError("connection removed from config")
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
	m.searchInput = m.searchQuery(m.searchTarget)
	m.searching = true
}

func (m *appModel) clearSearch(scope string) {
	selected, hadSelection := m.dashboardSelection()
	m.searching = false
	m.searchInput = ""
	if scope == searchHome {
		m.searchFilter = ""
		m.refreshHomeTable(true)
		if hadSelection {
			for index, summary := range m.visibleSummaries() {
				if configKey(summary.Account, summary.Name) == configKey(selected.Account, selected.Name) {
					m.home.SetCursor(index)
					break
				}
			}
		}
	} else {
		delete(m.searchFilters, scope)
		if scope == searchLogs {
			m.logOffset = 0
		}
	}
}

func (m appModel) updateSearch(key string, runes []rune) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.clearSearch(m.searchTarget)
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
					m.editor.cursor = editorCursorEnd(m.editor.input)
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
	width := m.homeTableWidth()
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
			m.setError("no saved connections yet — press n to scan for one")
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
		m.setError("no saved connections yet — press n to scan for one")
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
		m.setError("offline — " + action + " needs AWS connectivity (check network / credentials)")
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
			m.setError("logs: " + err.Error())
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
			m.setError("launch: " + err.Error())
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
	return &connectionEditor{cfg: cfg, originalName: cfg.Name, input: values[0], fields: []string{"name", "kind", "method", "region", "target", "ssh user", "ssh key", "ssh port", "vpc id", "subnet id", "instance id", "name tag", "routes", "dns mode", "dns resolvers", "dns domains", "dns to-ns", "manage resolver", "aws profile"}, values: values, profiles: profiles, regions: regions, routes: routes, savedValues: append([]string(nil), values...), savedRoutes: append([]config.NetworkRoute(nil), routes...)}
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
	if e.yamlMode {
		return m.updateYAMLEditor(key, runes)
	}
	if !e.routePicker && len(e.choices) == 0 {
		if key == "ctrl+s" || (!e.vimInsert && key == ":") {
			if key == "ctrl+s" {
				return m.saveVimEditor(false)
			}
			e.vimPrompt, e.vimInput = ":", ""
			return m, nil
		}
		var handled bool
		key, handled = e.vimKey(key, runes)
		if handled {
			return m, nil
		}
	}
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
				e.cursor = editorCursorEnd(e.input)
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
		e.cursor = max(editorCursorEnd(e.values[e.field])-1, 0)
	case "down", "tab":
		e.field = neighboringEditorField(e, m.searchQuery(searchEditor), 1)
		e.cursor = max(editorCursorEnd(e.values[e.field])-1, 0)
	case "left", "right":
		// Choice fields have no free-form caret; only the profile field
		// above uses left/right for cycling.
		if len(editorFieldChoices(e)) > 0 {
			break
		}
		if key == "left" && e.cursor > 0 {
			e.cursor--
		}
		last := utf8.RuneCountInString(e.input)
		if !e.vimInsert {
			last = max(last-1, 0)
		}
		if key == "right" && e.cursor < last {
			e.cursor++
		}
	case "home":
		e.cursor = 0
	case "end":
		e.cursor = utf8.RuneCountInString(e.input)
		if !e.vimInsert {
			e.cursor = max(e.cursor-1, 0)
		}
	case "ctrl+y":
		if err := enterYAMLEditor(e); err != nil {
			m.setError("yaml: " + err.Error())
		}
	case "v":
		if m.options.CheckAWSProfile != nil {
			cfg, err := editorConfig(e)
			if err != nil {
				m.setError("profile check: " + err.Error())
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
		if e.cursor > 0 {
			e.input = deleteRuneBefore(e.input, e.cursor)
			e.cursor--
			e.values[e.field] = e.input
		}
	case "delete":
		if e.cursor < utf8.RuneCountInString(e.input) {
			e.input = deleteRuneAt(e.input, e.cursor)
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
			e.cursor = max(editorCursorEnd(e.input)-1, 0)
			break
		}
		m.setError("press ctrl+s to save the connection")
	default:
		if len(runes) > 0 && len(editorFieldChoices(e)) == 0 {
			text := string(runes)
			e.input = insertAtRune(e.input, e.cursor, text)
			e.cursor += utf8.RuneCountInString(text)
			e.values[e.field] = e.input
		}
	}
	if m.editor != nil {
		e.input = e.values[e.field]
		if !e.vimInsert {
			e.cursor = min(e.cursor, max(editorCursorEnd(e.input)-1, 0))
		}
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
			m.setError("rescan: saving is unavailable")
			return m, nil
		}
		if position < 0 {
			m.setError("rescan: no candidates match the search")
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
				m.setError("select at least one route")
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

// editorYAMLDocument renders the editor's live draft as YAML, or the error
// that prevents rendering it.
func editorYAMLDocument(e *connectionEditor) (string, error) {
	cfg, err := editorConfig(e)
	if err != nil {
		return "", err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// enterYAMLEditor switches the editor into inline YAML mode, seeding the
// buffer from the current draft.
func enterYAMLEditor(e *connectionEditor) error {
	document, err := editorYAMLDocument(e)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(document, "\n"), "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	e.yamlMode = true
	e.yamlLines = lines
	e.yamlRow, e.yamlCol, e.yamlOffset = 0, 0, 0
	e.yamlError = ""
	e.yamlBaseline = strings.Join(lines, "\n")
	e.vimInsert = false
	e.vimUndo = nil
	return nil
}

// applyYAMLToEditor parses the YAML buffer and rebuilds the field editor from
// it. Identity/metadata (name, new/conflicted flags) is preserved.
func applyYAMLToEditor(e *connectionEditor) error {
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(strings.Join(e.yamlLines, "\n")), &cfg); err != nil {
		return err
	}
	rebuilt := newConnectionEditor(&cfg, e.profiles, e.regions)
	rebuilt.originalName = e.originalName
	rebuilt.isNew = e.isNew
	rebuilt.conflicted = e.conflicted
	rebuilt.field = max(min(e.field, len(rebuilt.fields)-1), 0)
	rebuilt.input = rebuilt.values[rebuilt.field]
	rebuilt.cursor = editorCursorEnd(rebuilt.input)
	*e = *rebuilt
	return nil
}

// yamlClampCol keeps the caret inside the current line after vertical motion.
func (e *connectionEditor) yamlClampCol() {
	if e.yamlRow < 0 {
		e.yamlRow = 0
	}
	if e.yamlRow >= len(e.yamlLines) {
		e.yamlRow = len(e.yamlLines) - 1
	}
	if e.yamlRow < 0 {
		e.yamlRow = 0
		return
	}
	width := utf8.RuneCountInString(e.yamlLines[e.yamlRow])
	if !e.vimInsert {
		width = max(width-1, 0)
	}
	if e.yamlCol > width {
		e.yamlCol = width
	}
	if e.yamlCol < 0 {
		e.yamlCol = 0
	}
}

// updateYAMLEditor drives the inline YAML editor and its Vim-style modes.
func (m appModel) updateYAMLEditor(key string, runes []rune) (tea.Model, tea.Cmd) {
	e := m.editor
	if key == "ctrl+s" {
		return m.saveVimEditor(false)
	}
	if !e.vimInsert && (key == ":" || key == "/") {
		e.vimPrompt, e.vimInput = key, ""
		return m, nil
	}
	var handled bool
	key, handled = e.vimKey(key, runes)
	if handled {
		return m, nil
	}
	e.yamlError = ""
	switch key {
	case "up":
		if e.yamlRow > 0 {
			e.yamlRow--
			e.yamlClampCol()
		}
	case "down":
		if e.yamlRow < len(e.yamlLines)-1 {
			e.yamlRow++
			e.yamlClampCol()
		}
	case "left":
		if e.yamlCol > 0 {
			e.yamlCol--
		} else if e.vimInsert && e.yamlRow > 0 {
			e.yamlRow--
			e.yamlCol = utf8.RuneCountInString(e.yamlLines[e.yamlRow])
		}
	case "right":
		last := utf8.RuneCountInString(e.yamlLines[e.yamlRow])
		if !e.vimInsert {
			last = max(last-1, 0)
		}
		if e.yamlCol < last {
			e.yamlCol++
		} else if e.vimInsert && e.yamlRow < len(e.yamlLines)-1 {
			e.yamlRow++
			e.yamlCol = 0
		}
	case "home":
		e.yamlCol = 0
	case "end":
		e.yamlCol = utf8.RuneCountInString(e.yamlLines[e.yamlRow])
		if !e.vimInsert {
			e.yamlCol = max(e.yamlCol-1, 0)
		}
	case "pgup":
		e.yamlRow = max(e.yamlRow-10, 0)
		e.yamlClampCol()
	case "pgdown":
		e.yamlRow = min(e.yamlRow+10, len(e.yamlLines)-1)
		e.yamlClampCol()
	case "backspace":
		line := e.yamlLines[e.yamlRow]
		if e.yamlCol > 0 {
			e.yamlLines[e.yamlRow] = deleteRuneBefore(line, e.yamlCol)
			e.yamlCol--
		} else if e.yamlRow > 0 {
			previous := e.yamlLines[e.yamlRow-1]
			e.yamlCol = utf8.RuneCountInString(previous)
			e.yamlLines[e.yamlRow-1] = previous + line
			e.yamlLines = append(e.yamlLines[:e.yamlRow], e.yamlLines[e.yamlRow+1:]...)
			e.yamlRow--
		}
	case "delete":
		line := e.yamlLines[e.yamlRow]
		if e.yamlCol < utf8.RuneCountInString(line) {
			e.yamlLines[e.yamlRow] = deleteRuneAt(line, e.yamlCol)
		} else if e.yamlRow < len(e.yamlLines)-1 {
			e.yamlLines[e.yamlRow] = line + e.yamlLines[e.yamlRow+1]
			e.yamlLines = append(e.yamlLines[:e.yamlRow+1], e.yamlLines[e.yamlRow+2:]...)
		}
	case "enter":
		line := []rune(e.yamlLines[e.yamlRow])
		if e.yamlCol > len(line) {
			e.yamlCol = len(line)
		}
		before, after := string(line[:e.yamlCol]), string(line[e.yamlCol:])
		e.yamlLines[e.yamlRow] = before
		tail := append([]string{after}, e.yamlLines[e.yamlRow+1:]...)
		e.yamlLines = append(e.yamlLines[:e.yamlRow+1], tail...)
		e.yamlRow++
		e.yamlCol = 0
	case "tab":
		e.yamlLines[e.yamlRow] = insertAtRune(e.yamlLines[e.yamlRow], e.yamlCol, "  ")
		e.yamlCol += 2
	default:
		if len(runes) > 0 {
			text := string(runes)
			e.yamlLines[e.yamlRow] = insertAtRune(e.yamlLines[e.yamlRow], e.yamlCol, text)
			e.yamlCol += utf8.RuneCountInString(text)
		}
	}
	e.yamlClampCol()
	return m, nil
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

// updateSudoPassword drives the sudo authorization modal: enter submits a
// non-empty password, esc/ctrl+c cancel with an empty reply. Replies go
// directly (the channel is buffered) so a timed-out asker can't wedge the
// UI.
func (m appModel) updateSudoPassword(key string, message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "ctrl+c":
		if m.sudoPrompt.authenticate {
			m.sudoPending = nil
			m.sudo = sudoMissing
			m.setError("sudo authorization cancelled")
		} else {
			m.sudoPrompt.request.Reply <- ""
		}
		m.sudoPrompt = nil
		return m, nil
	case "enter":
		value := m.sudoPrompt.input.Value()
		if strings.TrimSpace(value) == "" {
			return m, nil
		}
		if m.sudoPrompt.authenticate {
			m.sudoPrompt = nil
			return m, m.authenticateSudoCmd(value)
		}
		m.sudoPrompt.request.Reply <- value
		m.sudoPrompt = nil
		return m, nil
	}
	var command tea.Cmd
	m.sudoPrompt.input, command = m.sudoPrompt.input.Update(message)
	return m, command
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
	visible := max(m.chrome().Budget(m.height, m.width), 1)
	switch key {
	case "up", "k":
		m.logOffset = min(m.logOffset+1, max(len(visibleLines)-visible, 0))
	case "down", "j":
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
		m.setError("connection details are unavailable")
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
		input := []rune(m.shellInput)
		if len(input) > 0 {
			m.shellInput = string(input[:len(input)-1])
		}
		return m, nil
	case "tab":
		matches := m.matchingCommands()
		if len(matches) > 0 {
			m.shellInput = matches[0] + " "
		}
		return m, nil
	case "enter":
		outcome, err := ParseShell(m.shellInput, m.currentName(), m.connectionNames())
		if err != nil {
			m.setError(err.Error())
			return m, nil
		}
		if outcome.Quit {
			m.outcome.Quit = true
			return m, tea.Quit
		}
		if outcome.Action == "" {
			// help / bare command name: stay in the app, list the options.
			m.setError("commands: " + strings.Join(shellCommands, ", "))
			return m, nil
		}
		return m.requestAction(outcome.Action, outcome.Connection, outcome.View)
	}
	// Any unhandled key carrying runes (the runtime normalizes printable
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

// matchingCommands returns complete command lines matching the current input.
func (m appModel) matchingCommands() []string {
	input := m.shellInput
	candidates := append([]string(nil), shellCommands...)
	candidates = append(candidates, m.connectionNames()...)
	if strings.ContainsAny(input, " \t") {
		candidates = nil
		fields := strings.Fields(input)
		if len(fields) == 0 {
			return nil
		}
		if fields[0] == "dns" {
			for _, action := range []string{"install", "remove", "edit"} {
				command := "dns " + action
				candidates = append(candidates, command)
				for _, name := range m.connectionNames() {
					candidates = append(candidates, command+" "+name)
				}
			}
		} else {
			switch fields[0] {
			case "start", "stop", "launch", "logs", "status", "print", "rescan", "forget":
				for _, name := range m.connectionNames() {
					candidates = append(candidates, fields[0]+" "+name)
				}
			}
		}
	}
	var matches []string
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(input)) {
			matches = append(matches, candidate)
		}
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
// Home dashboard
// ---------------------------------------------------------------------------

// The dashboard is the persistent k9s-style header above the connections
// table: metadata on the left, keybindings in the middle, and the wordmark on
// the right. Unlike a startup splash it stays on screen with the table.
var (
	dashboardMetaLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	dashboardMetaValueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	dashboardKeyStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	dashboardKeyLabelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	dashboardLogoStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208"))
)

// dashboardLogoWord is the wordmark shown at the left of the top bar.
var dashboardLogoWord = "BASTIONCTL"

type dashboardMetaRow struct{ Label, Value string }

// dashboardSelection is the connection summarized in the dashboard.
func (m appModel) dashboardSelection() (ConnSummary, bool) {
	if m.level() == 0 {
		summaries := m.visibleSummaries()
		if cursor := m.home.Cursor(); cursor >= 0 && cursor < len(summaries) {
			return summaries[cursor], true
		}
		return ConnSummary{}, false
	}
	name := m.currentName()
	if name != "" {
		summaries := m.visibleSummaries()
		if cursor := m.home.Cursor(); cursor >= 0 && cursor < len(summaries) && summaries[cursor].Name == name {
			return summaries[cursor], true
		}
		for _, summary := range m.options.Summaries {
			if summary.Name == name {
				return summary, true
			}
		}
	}
	if index := m.options.Highlight; index >= 0 && index < len(m.options.Summaries) {
		return m.options.Summaries[index], true
	}
	return ConnSummary{}, false
}

func (m appModel) dashboardMetaRows() []dashboardMetaRow {
	profile := m.options.Profile
	if profile == "" {
		profile = "default"
	}
	running := 0
	for _, summary := range m.options.Summaries {
		if summary.Running {
			running++
		}
	}
	rows := []dashboardMetaRow{
		{"Profile", profile},
		{"Connections", fmt.Sprintf("%d saved · %d running", len(m.options.Summaries), running)},
	}
	selected, hasSelection := m.dashboardSelection()
	account := m.options.Account
	if hasSelection && selected.Account != "" {
		account = selected.Account
		if label := m.options.AccountLabels[selected.Account]; label != "" {
			account = label
		}
	}
	if account == "" {
		for _, summary := range m.options.Summaries {
			if summary.Account != "" {
				account = "—"
				break
			}
		}
	}
	if account != "" {
		rows = append(rows, dashboardMetaRow{"Account", account})
	}
	if summary, ok := selected, hasSelection; ok {
		rows = append(rows, dashboardMetaRow{"Selected", summary.Name})
		method := summary.Method
		if method == "" {
			method = "—"
		}
		if summary.Region != "" {
			method += " · " + summary.Region
		}
		rows = append(rows, dashboardMetaRow{"Method", method})
		dns := summary.DNS
		if dns == "" {
			dns = "off"
		}
		rows = append(rows, dashboardMetaRow{"DNS", dns})
	} else {
		rows = append(rows, dashboardMetaRow{"Selected", "—"}, dashboardMetaRow{"Method", "—"}, dashboardMetaRow{"DNS", "—"})
	}
	if m.options.Offline {
		rows = append(rows, dashboardMetaRow{"Mode", "offline · cached"})
	}
	version := m.options.Version
	if version == "" {
		version = "dev"
	}
	rows = append(rows, dashboardMetaRow{"Bastionctl", version})
	return rows
}

func padRight(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// renderDashboardMeta renders the metadata block. valueCap bounds the value
// column so a long label cannot overflow the terminal; the caller normally
// passes the full available width so nothing is cut short.
func renderDashboardMeta(m appModel, valueCap int) string {
	rows := m.dashboardMetaRows()
	labelWidth := 0
	for _, row := range rows {
		labelWidth = max(labelWidth, lipgloss.Width(row.Label))
	}
	valueWidth := 0
	for _, row := range rows {
		valueWidth = max(valueWidth, lipgloss.Width(row.Value))
	}
	valueWidth = min(valueWidth, max(valueCap-labelWidth-2, 1))
	lines := make([]string, len(rows))
	for index, row := range rows {
		label := dashboardMetaLabelStyle.Render(padRight(row.Label, labelWidth))
		value := dashboardMetaValueStyle.Render(truncate(row.Value, valueWidth))
		lines[index] = label + "  " + value
	}
	return strings.Join(lines, "\n")
}

// renderDashboardLines returns the persistent connection metadata rows.
func renderDashboardLines(m appModel, width int) []string {
	if width <= 0 {
		width = 80
	}
	meta := renderDashboardMeta(m, max(width-2, 20))
	rows := strings.Split(meta, "\n")
	out := make([]string, len(rows))
	for index, row := range rows {
		out[index] = " " + row
	}
	return out
}

// dashboardMinWidth/dashboardMinHeight keep the header from crowding out the
// table on small terminals; below them the compact chrome header is used.
const (
	dashboardMinWidth  = 60
	dashboardMinHeight = 16
)

func (m appModel) dashboardEligible() bool {
	if m.width < dashboardMinWidth {
		return false
	}
	if m.height < dashboardMinHeight {
		return false
	}
	return true
}

// dashboardActive reports whether the current view shows connection metadata.
func (m appModel) dashboardActive() bool {
	return m.dashboardEligible()
}

// homeTableWidth is the table width inside the home panel.
func (m appModel) homeTableWidth() int {
	width := m.width
	if width <= 0 {
		width = 120
	}
	if m.level() == 0 {
		return max(width-2, 20)
	}
	return width
}

// homeTableTitle is the label embedded in the table panel's top border,
// e.g. connections(all)[6].
func (m appModel) homeTableTitle() string {
	filter := "all"
	if query := strings.TrimSpace(m.searchQuery(searchHome)); query != "" {
		filter = query
	}
	return fmt.Sprintf("connections(%s)[%d]", filter, len(m.visibleSummaries()))
}

var (
	tablePanelBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	tablePanelTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
)

// renderPanel wraps content in the k9s-style bordered panel with a title in
// the top border. height is the number of inner rows; the panel adds the two
// border rows. Every screen uses this frame so panels share one look.
func renderPanel(content string, width, height int, title string) string {
	width = max(width, 4)
	inner := width - 2
	title = truncate(title, max(inner-2, 1))
	rules := max(inner-lipgloss.Width(title)-2, 0)
	leftRules := rules / 2
	rightRules := rules - leftRules
	top := tablePanelBorderStyle.Render("┌"+strings.Repeat("─", leftRules)+" ") +
		tablePanelTitleStyle.Render(title) +
		tablePanelBorderStyle.Render(" "+strings.Repeat("─", rightRules)+"┐")
	if inner < 3 {
		top = tablePanelBorderStyle.Render("┌" + strings.Repeat("─", inner) + "┐")
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if height > 0 {
		if len(lines) > height {
			lines = lines[:height]
		}
		for len(lines) < height {
			lines = append(lines, "")
		}
	}
	out := make([]string, 0, len(lines)+2)
	out = append(out, top)
	for _, line := range lines {
		out = append(out, tablePanelBorderStyle.Render("│")+padCell(line, inner)+tablePanelBorderStyle.Render("│"))
	}
	out = append(out, tablePanelBorderStyle.Render("└"+strings.Repeat("─", inner)+"┘"))
	return strings.Join(out, "\n")
}

// renderTablePanel is the home table's panel.
func renderTablePanel(table string, width, height int, title string) string {
	return renderPanel(table, width, height, title)
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
	budget := m.chrome().Budget(m.height, m.width)
	// Every screen body sits inside the same bordered panel, so renderers
	// get two fewer rows and two fewer columns.
	frameWidth := m.width
	if frameWidth <= 0 {
		frameWidth = 80
	}
	innerBudget := budget
	panelHeight := 0
	if budget != maxInt {
		innerBudget = max(budget-2, 1)
		panelHeight = innerBudget
	}
	innerWidth := max(frameWidth-2, 4)

	var base string
	switch {
	case m.logSession != nil:
		query := m.searchQuery(searchLogs)
		lines := filterAndHighlightLines(m.logLines, query)
		content := ""
		if len(lines) == 0 && len(m.logLines) > 0 && query != "" {
			content = "No log lines match " + query
		} else {
			content = renderLogView(lines, m.logOffset, innerBudget, m.logDone, m.logError)
		}
		title := m.logTitle
		if title == "" {
			title = "logs(" + m.currentName() + ")"
		}
		base = renderPanel(content, frameWidth, panelHeight, title)
	case m.newWizard != nil:
		content := renderNewWizard(m.newWizard, innerBudget, innerWidth, m.searchQuery(m.activeSearchScope()))
		base = renderPanel(content, frameWidth, panelHeight, "new connection · "+m.newWizard.step)
	case m.rescan != nil:
		content := renderRescanPicker(m.rescan, innerBudget, innerWidth, m.searchQuery(searchRescan))
		base = renderPanel(content, frameWidth, panelHeight, "rescan("+m.rescan.connection+")")
	case m.editor != nil:
		scope := searchEditor
		if len(m.editor.choices) > 0 {
			scope = searchEditorChoices
		}
		title := "edit(" + m.editor.originalName + ")"
		if m.editor.isNew {
			title = "new connection"
		}
		if m.editor.yamlMode {
			title = "yaml · " + m.editor.originalName
		}
		var content string
		switch {
		case m.editor.yamlMode:
			content = renderYAMLEditor(m.editor, innerBudget, innerWidth)
		case m.width >= editorSplitMinWidth:
			content = renderEditorSplit(m.editor, innerBudget, innerWidth, m.searchQuery(scope))
		default:
			content = renderEditor(m.editor, innerBudget, m.searchQuery(scope), innerWidth)
		}
		base = renderPanel(content, frameWidth, panelHeight, title)
	case m.level() == 1:
		content := renderFilteredDetailCard(m.options.Detail, innerBudget, m.searchQuery(searchDetail))
		name := "connection"
		if m.options.Detail != nil && m.options.Detail.Name != "" {
			name = "connection(" + m.options.Detail.Name + ")"
		}
		base = renderPanel(content, frameWidth, panelHeight, name)
	case m.width == 0:
		// First frame, before the terminal size arrives.
		base = ""
	default:
		m.home.SetHeight(max(innerBudget, 2))
		base = renderPanel(m.home.View(), frameWidth, panelHeight, m.homeTableTitle())
	}
	if (m.help || m.actionModal != nil || m.sudoPrompt != nil) && budget != maxInt {
		lines := strings.Count(base, "\n") + 1
		if lines < budget {
			base += strings.Repeat("\n", budget-lines)
		}
	}
	if m.help {
		return overlayCenter(base, helpPanel(m.width, budget, m.editor != nil), m.width)
	}
	if m.sudoPrompt != nil {
		return overlayCenter(base, renderSudoPasswordModal(m.sudoPrompt, m.width, budget), m.width)
	}
	if m.editor != nil && m.editor.discardConfirm {
		modal := &actionModal{title: "Discard unsaved changes?", body: "y discard · n keep editing", running: true}
		return overlayCenter(base, renderActionModal(modal, m.width, budget), m.width)
	}
	if m.actionModal != nil {
		return overlayCenter(base, renderActionModal(m.actionModal, m.width, budget), m.width)
	}
	return base
}

func renderNewWizard(wizard *newConnectionWizard, budget, width int, query string) string {
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
		budget = len(indices) + 2
	}
	rowBudget := max(budget-2, 1)
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
			row = selectionStyle.Render(row)
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
	rowBudget := max(budget-2, 1)
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
			row = selectionStyle.Render(row)
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

// editorSplitMinWidth is the narrowest terminal that shows the field editor
// and the YAML preview side by side. Narrower terminals keep the full-width
// field editor; ctrl+y still opens the YAML editor.
const editorSplitMinWidth = 90

// padCell truncates s to width display cells and pads it with spaces so the
// next column starts at a fixed offset.
func padCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncate(s, width)
	if gap := width - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// colorizeYAMLLine highlights YAML tokens in a preview line.
func colorizeYAMLLine(line string) string {
	return renderVimLine(line, -1, false, true, 0)
}

// renderEditorSplit composes the field editor and a read-only YAML preview
// side by side, separated by a vertical rule.
func renderEditorSplit(editor *connectionEditor, budget, width int, query string) string {
	leftWidth := width * 3 / 5
	leftWidth = max(min(leftWidth, width-28), 44)
	rightWidth := max(width-leftWidth-1, 1)
	left := strings.Split(renderEditor(editor, budget, query, leftWidth), "\n")
	right := strings.Split(renderYAMLPreview(editor, budget), "\n")
	rows := max(len(left), len(right))
	if budget > 0 && budget != maxInt {
		rows = min(rows, budget)
	}
	separator := chromeDimStyle.Render("│")
	lines := make([]string, 0, rows)
	for index := 0; index < rows; index++ {
		leftLine, rightLine := "", ""
		if index < len(left) {
			leftLine = left[index]
		}
		if index < len(right) {
			rightLine = right[index]
		}
		lines = append(lines, padCell(leftLine, leftWidth)+separator+padCell(rightLine, rightWidth))
	}
	return strings.Join(lines, "\n")
}

// renderYAMLPreview renders the live draft as YAML. Invalid drafts (e.g. a
// half-typed SSH port) show the validation error instead of a document.
func renderYAMLPreview(editor *connectionEditor, budget int) string {
	lines := []string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("YAML PREVIEW"),
		chromeDimStyle.Render("ctrl+y edit"),
	}
	document, err := editorYAMLDocument(editor)
	if err != nil {
		return strings.Join(append(lines, chromeErrStyle.Render(err.Error())), "\n")
	}
	body := strings.Split(strings.TrimRight(document, "\n"), "\n")
	total := len(body)
	limit := budget - len(lines)
	if budget <= 0 || budget == maxInt {
		limit = total
	}
	if limit < 1 {
		limit = 1
	}
	truncated := total > limit
	if truncated {
		body = body[:limit]
	}
	for _, line := range body {
		lines = append(lines, colorizeYAMLLine(line))
	}
	if truncated {
		lines[len(lines)-1] = chromeDimStyle.Render(fmt.Sprintf("… %d more lines", total-limit))
	}
	return strings.Join(lines, "\n")
}

// renderYAMLEditor renders the inline YAML editor with a scrolling viewport.
func renderYAMLEditor(editor *connectionEditor, budget, width int) string {
	mode := "NORMAL"
	if editor.vimInsert {
		mode = "INSERT"
	}
	lines := []string{
		chromeDimStyle.Render(mode + " · :w save · :q close"),
	}
	reserve := 0
	if editor.yamlError != "" {
		reserve = 1
	}
	rows := budget - len(lines) - reserve
	if budget <= 0 || budget == maxInt {
		rows = len(editor.yamlLines)
	}
	if rows < 1 {
		rows = 1
	}
	if editor.yamlRow < editor.yamlOffset {
		editor.yamlOffset = editor.yamlRow
	}
	if editor.yamlRow >= editor.yamlOffset+rows {
		editor.yamlOffset = editor.yamlRow - rows + 1
	}
	if editor.yamlOffset < 0 {
		editor.yamlOffset = 0
	}
	end := min(editor.yamlOffset+rows, len(editor.yamlLines))
	for index := editor.yamlOffset; index < end; index++ {
		cursor := -1
		if index == editor.yamlRow {
			cursor = editor.yamlCol
		}
		lines = append(lines, renderVimLine(editor.yamlLines[index], cursor, editor.vimInsert, true, max(width, 1)))
	}
	if editor.yamlError != "" {
		lines = append(lines, chromeErrStyle.Render(truncate(editor.yamlError, max(width, 1))))
	}
	return strings.Join(lines, "\n")
}

func renderEditor(editor *connectionEditor, budget int, query string, widths ...int) string {
	if editor.routePicker {
		indices := matchingEditorRoutes(editor.routes, query)
		if budget <= 0 || budget == maxInt {
			budget = len(indices) + 2
		}
		lines := []string{
			chromeDimStyle.Render("↑/↓ move · space toggle · a all · m add CIDR · enter apply · esc discard"),
			"",
		}
		if editor.addingRoute {
			lines = append(lines, "Additional CIDR: "+editor.routeInput+"|")
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
				row = selectionStyle.Render("▶ " + highlightText(row, query))
			} else {
				row = highlightText(row, query)
			}
			lines = append(lines, row)
		}
		return strings.Join(lines, "\n")
	}
	if len(editor.choices) > 0 {
		lines := []string{
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
				row = selectionStyle.Render("▶ " + choice)
			}
			lines = append(lines, row)
		}
		return strings.Join(lines, "\n")
	}
	if budget <= 0 || budget == maxInt {
		budget = len(editor.fields) + 3
	}
	indices := matchingEditorFields(editor, query)
	if len(indices) == 0 {
		return "No matching fields"
	}
	activePosition := indexOf(indices, editor.field)
	if activePosition < 0 {
		activePosition = 0
	}
	fieldRows := max(budget-3, 1)
	start := max(activePosition-fieldRows+1, 0)
	if lastStart := max(len(indices)-fieldRows, 0); start > lastStart {
		start = lastStart
	}
	end := min(start+fieldRows, len(indices))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	active := selectionStyle
	mode := "NORMAL"
	if editor.vimInsert {
		mode = "INSERT"
	}
	width := 0
	if len(widths) > 0 {
		width = max(widths[0]-19, 1)
	}
	lines := []string{
		muted.Render(fmt.Sprintf("%s · Field %d/%d · %s", mode, editor.field+1, len(editor.fields), editor.fields[editor.field])),
		muted.Render("j/k move · i insert · enter options · ctrl+y yaml · :w save"),
		"",
	}
	if editor.conflicted {
		lines[0] = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).Render("Changed on disk · Esc to cancel and reopen")
	}
	for position := start; position < end; position++ {
		i := indices[position]
		value := editor.values[i]
		if i == editor.field {
			label := active.Render(fmt.Sprintf("▶ %-16s", editor.fields[i]))
			lines = append(lines, label+" "+renderVimLine(value, editor.cursor, editor.vimInsert, false, width))
		} else {
			lines = append(lines, "  "+highlightText(fmt.Sprintf("%-16s %s", editor.fields[i], value), query))
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

// renderSudoPasswordModal draws the in-app sudo authorization popup: the
// reason line (which attempt, what failed last), a masked input, and the
// key hints. Touch ID never goes through here — sudo's PAM chain handles
// it before the typed-password fallback fires.
func renderSudoPasswordModal(prompt *sudoPasswordPrompt, width, height int) string {
	innerWidth := min(max(width-8, 24), 78)
	lines := []string{lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("Sudo authorization")}
	for _, line := range strings.Split(strings.TrimSpace(prompt.request.Reason), "\n") {
		lines = append(lines, truncate(chromeDimStyle.Render(line), innerWidth))
	}
	lines = append(lines, "", "Password: "+prompt.input.View(), "", chromeDimStyle.Render("enter submit · esc cancel"))
	if height > 2 && len(lines) > height-2 {
		lines = lines[:height-2]
	}
	return helpBorderStyle.Render(strings.Join(lines, "\n"))
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
func helpPanel(width, height int, editing bool) string {
	lines := helpPanelLines(width)
	if editing {
		lines = editorHelpPanelLines(width)
	}
	// Account for the two border rows.
	if height > 2 && len(lines) > height-2 {
		lines = lines[:height-2]
	}
	return helpBorderStyle.Render(strings.Join(lines, "\n"))
}

func editorHelpPanelLines(width int) []string {
	inner := max(min(width-6, 78), 1)
	lines := []string{
		"Editor",
		"j/k or arrows    move between fields or lines",
		"h/l or arrows    move cursor",
		"i / a            insert before / after cursor",
		"o / x / dd       new line / delete character / line",
		"u                undo",
		"ctrl+o           return to normal mode",
		"esc              return; confirm unsaved changes",
		"enter            choose options",
		"ctrl+y           edit YAML",
		"/                search fields or YAML",
		":w / ctrl+s      save to disk",
		":wq              save and close",
		":q / :q!         close / discard unsaved changes",
		"v                check AWS profile",
	}
	for index := range lines {
		lines[index] = truncate(lines[index], inner)
	}
	return lines
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
		row("j/k · ↑/↓", "move the selection · pgup/pgdn page"),
		row("enter · esc", "open · go back"),
		row("/ · :", "filter (esc clears) · commands (tab completes)"),
		row("?", "toggle this help · q quits"),
		section("Actions — home"),
		row("n · s · x", "new (needs AWS) · start · stop"),
		row("a · l · i", "launch · live logs · status"),
		row("d · e · v", "DNS · edit connection · check AWS"),
		row("r · f", "rescan · forget"),
		section("Actions — connection view"),
		row("p · 1 · 2", "print command · install/remove DNS resolver"),
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
		if len(chrome.Dashboard) > 0 {
			chrome.Loading = "loading " + m.loadingDetail + "…"
		} else {
			chrome.Info = "loading " + m.loadingDetail + "…"
		}
	}
	if m.editor != nil {
		chrome.Title = "edit(" + m.editor.originalName + ")"
		if m.editor.isNew {
			chrome.Title = "new connection"
		}
		if m.editor.yamlMode {
			chrome.Title = "yaml · " + m.editor.originalName
		}
		if m.editor.routePicker {
			chrome.Hints = []Hint{{"↑/↓", "move"}, {"space", "toggle"}, {"a", "all"}, {"m", "add CIDR"}, {"enter", "apply"}, {"/", "search"}, {"esc", "discard"}}
		} else if m.editor.yamlMode {
			chrome.Hints = []Hint{{"i", "insert"}, {"ctrl+o", "normal"}, {"esc", "back"}, {":w", "save"}, {"/", "search"}, {"?", "help"}}
		} else if len(m.editor.choices) > 0 {
			chrome.Hints = []Hint{{"↑/↓", "choose"}, {"enter", "select"}, {"esc", "cancel"}}
		} else {
			chrome.Hints = []Hint{{"j/k", "move"}, {"i", "insert"}, {"enter", "options"}, {"ctrl+y", "yaml"}, {"ctrl+o", "normal"}, {"esc", "back"}, {":w", "save"}, {"?", "help"}}
		}
		if m.editor.vimPrompt != "" {
			chrome.ShellOpen = true
			chrome.Shell = m.editor.vimInput
			chrome.ShellPrefix = m.editor.vimPrompt
		}
	}
	if m.newWizard != nil {
		chrome.Title = "new connection · " + m.newWizard.step
		chrome.Info = m.newWizard.step
		chrome.Hints = []Hint{{"↑/↓", "select"}, {"enter", "continue"}, {"/", "search"}, {"esc", "back"}}
		if m.newWizard.step == "routes" {
			chrome.Hints = []Hint{{"↑/↓", "move"}, {"space", "toggle"}, {"a", "all"}, {"enter", "continue"}, {"/", "search"}, {"esc", "back"}}
		}
	}
	if m.rescan != nil {
		chrome.Title = "rescan · " + m.rescan.connection
		chrome.Info = fmt.Sprintf("%d/%d candidates", m.rescan.cursor+1, len(m.rescan.candidates))
		chrome.Hints = []Hint{{"↑/↓", "select pivot"}, {"enter", "save"}, {"esc", "cancel"}}
	}
	if m.logSession != nil {
		chrome.Title = m.logTitle
		chrome.Info = "streaming"
		if m.logDone {
			chrome.Info = "ended"
		}
		chrome.Hints = []Hint{{"esc", "close"}, {"q", "close"}, {"j/k", "scroll"}, {"pgup/pgdn", "page"}}
	}
	if m.help {
		chrome.Hints = []Hint{{"q", "close"}}
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
	home := m.homeChrome()
	home.Dashboard = chrome.Dashboard
	comparison := m
	comparison.options.Detail = &ConnDetail{DNSWantsInstall: true}
	connection := comparison.connectionChrome()
	connection.Dashboard = chrome.Dashboard
	chrome.MinHeaderRows = max(len(home.headerRows(m.width, m.height)), len(connection.headerRows(m.width, m.height)))
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
	title := "connections"
	if m.options.Profile != "" {
		title += " · profile " + m.options.Profile
	}
	if m.options.Account != "" {
		title += " · " + m.options.Account
	}
	chrome := Chrome{Title: title, Info: info, Hints: hints, Error: m.notice()}
	if m.dashboardActive() {
		chrome.Dashboard = renderDashboardLines(m, m.width)
	}
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
	title := fmt.Sprintf("%s · %s/%s · %s", detail.Name, detail.Kind, detail.Method, detail.Region)
	chrome := Chrome{Title: title, Info: info, Hints: hints, Error: m.notice()}
	if m.dashboardActive() {
		chrome.Dashboard = renderDashboardLines(m, m.width)
	}
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
		{title: "Status", cellOf: func(s ConnSummary) string {
			if s.Running && s.Stale {
				return "◐ DNS missing"
			}
			return s.StatusLabel()
		}, minW: 12, capW: 16, rank: 0},
		{title: "Connection", cellOf: func(s ConnSummary) string { return s.Name }, minW: 12, capW: 28, rank: 0},
		{title: "Region", cellOf: (func(s ConnSummary) string { return s.Region }), minW: 8, capW: 15, rank: 1},
		{title: "AWS Profile", cellOf: func(s ConnSummary) string { return s.AWSProfileLabel() }, minW: 12, capW: 36, rank: 2},
		{title: "DNS", cellOf: func(s ConnSummary) string { return s.DNS }, minW: 12, capW: 24, rank: 3},
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
		// Tiny terminal: even the essentials overflow; let the table
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
