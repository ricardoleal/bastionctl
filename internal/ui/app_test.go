package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bastionctl/internal/config"
)

func runesOf(s string) []rune { return []rune(s) }

func newTestApp(summaries []ConnSummary, options RunOptions) appModel {
	if options.Summaries == nil {
		options.Summaries = summaries
	}
	return appModel{options: options, home: newHomeTable(options.Summaries, options.Highlight)}
}

func TestHomeViewContainsRowsOffline(t *testing.T) {
	summaries := []ConnSummary{
		{Name: "dns-split", Account: "698715940080", Kind: "vpc", Method: "ssm", Region: "eu-central-1", AWSProfile: "bastionctl-test-profile-one", Networks: "10.0.0.0/16", DNS: "off", Running: true, PID: 2431},
		{Name: "corp", Account: "698715940080", Kind: "vpc", Method: "ssh", Region: "eu-west-1", Networks: "172.16.0.0/12", DNS: "off"},
	}
	model := newTestApp(summaries, RunOptions{Offline: true})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 180, Height: 24})
	model = updated.(appModel)
	view := model.View()
	for _, expected := range []string{"dns-split", "corp", "Running", "Idle", "offline", "connections", "bastionctl-test-profile-one"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("home view missing %q:\n%s", expected, view)
		}
	}
}

func TestAWSProfileLabelUsesDefaultWhenUnset(t *testing.T) {
	if got := (ConnSummary{}).AWSProfileLabel(); got != "default" {
		t.Fatalf("AWSProfileLabel() = %q, want default", got)
	}
}

func TestShellTypingProducesOutcome(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})

	step := func(key string, runes []rune) {
		updated, _ := model.updateShell(key, runes)
		model = updated.(appModel)
	}
	step(":", nil)
	step("stop", runesOf("stop"))
	step(" ", runesOf(" "))
	step("corp", runesOf("corp"))
	var command tea.Cmd
	updated, command := model.updateShell("enter", nil)
	model = updated.(appModel)
	if model.outcome.Action != ActStop || model.outcome.Connection != "corp" || command == nil {
		t.Fatalf("outcome = %+v, cmd nil = %v", model.outcome, command == nil)
	}
}

func TestShellExitAliasQuits(t *testing.T) {
	for _, command := range []string{"quit", "exit", "q"} {
		outcome, err := ParseShell(command, "", nil)
		if err != nil || !outcome.Quit {
			t.Errorf("ParseShell(%q) = %+v, %v; want quit", command, outcome, err)
		}
	}
}

func TestColonConnectionAliasOpensConnection(t *testing.T) {
	outcome, err := ParseShell("corp", "", []string{"Corp"})
	if err != nil || outcome.Action != ActOpen || outcome.Connection != "Corp" || outcome.View != ViewConnection {
		t.Fatalf("ParseShell connection alias = %+v, %v", outcome, err)
	}
}

func TestColonConnectionAliasStaysInProgram(t *testing.T) {
	loader := func(name string) (*ConnDetail, error) { return &ConnDetail{Name: name}, nil }
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{LoadDetail: loader})
	model.shell = true
	model.shellInput = "corp"
	updated, command := model.updateShell("enter", nil)
	model = updated.(appModel)
	if command == nil || model.outcome.Action != "" {
		t.Fatalf("alias open: outcome = %+v, cmd nil = %v", model.outcome, command == nil)
	}
	updated, followup := model.Update(command())
	model = updated.(appModel)
	if followup != nil || model.level() != 1 || model.currentName() != "corp" {
		t.Fatalf("alias detail load: level = %d, name = %q", model.level(), model.currentName())
	}
}

func TestDetailLoadFailureStaysInProgram(t *testing.T) {
	loader := func(string) (*ConnDetail, error) { return nil, errors.New("probe failed") }
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{LoadDetail: loader})
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
	model = updated.(appModel)
	if command == nil || !strings.Contains(model.View(), "loading corp") {
		t.Fatalf("loading state missing: cmd nil = %v, view = %q", command == nil, model.View())
	}
	updated, followup := model.Update(command())
	model = updated.(appModel)
	if followup != nil || model.level() != 0 || model.loadingDetail != "" || !strings.Contains(model.errorMsg, "probe failed") {
		t.Fatalf("failed detail load: level = %d, loading = %q, error = %q", model.level(), model.loadingDetail, model.errorMsg)
	}
}

func TestSlashSearchFiltersAndOpensConnection(t *testing.T) {
	summaries := []ConnSummary{
		{Name: "corp", Region: "eu-west-1"},
		{Name: "analytics", Region: "eu-central-1"},
	}
	loader := func(name string) (*ConnDetail, error) { return &ConnDetail{Name: name}, nil }
	model := newTestApp(summaries, RunOptions{LoadDetail: loader})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("central")})
	model = updated.(appModel)
	if model.currentName() != "analytics" || !strings.Contains(model.View(), "analytics") || strings.Contains(model.View(), "corp") {
		t.Fatalf("search results = %q; current = %q", model.View(), model.currentName())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if command == nil || model.outcome.Action != "" || model.outcome.Connection != "analytics" {
		t.Fatalf("search selection outcome = %+v, cmd nil = %v", model.outcome, command == nil)
	}
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if model.level() != 1 || model.currentName() != "analytics" {
		t.Fatalf("search detail load: level = %d, name = %q", model.level(), model.currentName())
	}
}

func TestSlashSearchFiltersConnectionDetails(t *testing.T) {
	detail := &ConnDetail{Name: "corp", Kind: "vpc", Method: "ssm", Region: "eu-central-1", Target: "i-123", TargetOK: true, SSHPort: 22}
	model := newTestApp(nil, RunOptions{Level: 1, Detail: detail})
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("target")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(appModel)
	view := model.View()
	if !strings.Contains(view, "i-123") || strings.Contains(view, "DNS") || model.searchFilters[searchDetail] != "target" {
		t.Fatalf("connection search did not filter the detail panel: %q", view)
	}
}

func TestSlashSearchFiltersEditorFieldsAndChoices(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	model := newTestApp(nil, RunOptions{AWSProfiles: []string{"default"}})
	model.editor = newConnectionEditor(cfg, model.options.AWSProfiles)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dns")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if !strings.Contains(model.View(), "dns mode") || strings.Contains(model.View(), "ssh port") {
		t.Fatalf("editor field search was not applied: %q", model.View())
	}
	model.editor.field = 13
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("split")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if !strings.Contains(model.View(), "split") || strings.Contains(model.View(), "all") || model.editor.choiceCursor != 2 {
		t.Fatalf("choice search did not filter/select DNS mode: view=%q cursor=%d", model.View(), model.editor.choiceCursor)
	}
}

func TestSlashSearchFiltersRescanCandidates(t *testing.T) {
	model := newTestApp(nil, RunOptions{})
	model.rescan = &rescanSelector{connection: "corp", candidates: []RescanCandidate{
		{Name: "analytics", InstanceID: "i-analytics", Method: "ssm", Target: "i-analytics"},
		{Name: "operations", InstanceID: "i-operations", Method: "direct_ip", Target: "10.0.0.2"},
	}}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("operations")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(appModel)
	view := model.View()
	if !strings.Contains(view, "i-operations") || strings.Contains(view, "i-analytics") {
		t.Fatalf("rescan candidate search failed: %q", view)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if model.rescan == nil || model.rescan.candidates[model.rescan.cursor].InstanceID != "i-operations" {
		t.Fatalf("search did not select the matching candidate: %+v", model.rescan)
	}
}

func TestSlashSearchFiltersAndHighlightsLogs(t *testing.T) {
	model := newTestApp(nil, RunOptions{})
	model.logSession = &LogSession{Lines: make(chan string), Cancel: func() {}}
	model.logLines = []string{"INFO connected", "ERROR target unavailable"}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("error")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	if strings.Contains(view, "INFO connected") || !strings.Contains(view, "ERROR target unavailable") {
		t.Fatalf("log search/filter/highlight failed: %q", view)
	}
}

func TestHomeHintsIncludeLaunch(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
	view := model.chrome().View("", 120, 24)
	if !strings.Contains(view, "launch") {
		t.Fatalf("home hints missing launch action:\n%s", view)
	}
}

func TestShellRejectsUnknownConnection(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
	updated, _ := model.updateShell(":", nil)
	model = updated.(appModel)
	updated, _ = model.updateShell("stop nope", runesOf("stop nope"))
	model = updated.(appModel)
	updated, _ = model.updateShell("enter", nil)
	model = updated.(appModel)
	if model.outcome.Action != "" || model.errorMsg == "" {
		t.Fatalf("outcome = %+v, errorMsg = %q", model.outcome, model.errorMsg)
	}
}

func TestShellOpenRendersInputBar(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	model = updated.(appModel)
	if !model.shell {
		t.Fatal(": must open the command line")
	}
	view := model.View()
	if !strings.Contains(view, "▏") {
		t.Fatalf("empty input bar missing from view:\n%s", view)
	}
	for _, expected := range []string{"start", "stop", "rescan"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("suggestion line missing %q:\n%s", expected, view)
		}
	}
	// Typed characters show up immediately.
	updated, _ = model.updateShell("stop", runesOf("stop"))
	model = updated.(appModel)
	if view := model.View(); !strings.Contains(view, "stop") || !strings.Contains(view, "▏") {
		t.Fatalf("typed input missing:\n%s", view)
	}
}

func TestShellErrorsShowWhileShellOpen(t *testing.T) {
	// A parse error must not hide the input bar: the error replaces the
	// suggestion line while the shell stays open.
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
	model.shell = true
	updated, _ := model.updateShell(":", nil)
	model = updated.(appModel)
	updated, _ = model.updateShell("frobnicate", runesOf("frobnicate"))
	model = updated.(appModel)
	updated, _ = model.updateShell("enter", nil)
	model = updated.(appModel)
	if model.errorMsg == "" {
		t.Fatal("expected a parse error")
	}
	view := model.View()
	if !strings.Contains(view, "unknown command") {
		t.Fatalf("error line missing from view:\n%s", view)
	}
	if !strings.Contains(view, "frobnicate") || !strings.Contains(view, "▏") {
		t.Fatalf("input bar missing from view:\n%s", view)
	}
}

func TestHelpOverlayTogglesAndSwallowsKeys(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	model = updated.(appModel)
	if !model.help {
		t.Fatal("? must open the help overlay")
	}
	view := model.View()
	for _, expected := range []string{"Navigation", "Actions — home", "Shell commands", "Status dots", "q quits"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("help overlay missing %q:\n%s", expected, view)
		}
	}
	// The frame must stay within the terminal height while overlaid.
	if lines := strings.Count(view, "\n") + 1; lines > 30 {
		t.Fatalf("help frame exceeded terminal height: %d lines", lines)
	}

	// Any key dismisses it; the key must not leak into the app (e.g. "q"
	// must not quit while the help is showing).
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	model = updated.(appModel)
	if model.help || command != nil {
		t.Fatalf("help close: help = %v, cmd = %v", model.help, command != nil)
	}
	if model.outcome.Quit {
		t.Fatal("q while help is open must not quit the app")
	}
	// The overlay is gone from the next frame.
	if view := model.View(); strings.Contains(view, "Actions — home") {
		t.Fatalf("help overlay still rendered after dismissal:\n%s", view)
	}
}

func TestOfflineGuardsNetworkActions(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{Offline: true})

	// rescan hotkey must be refused offline and stay in the app.
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}, "r")
	model = updated.(appModel)
	if model.outcome.Action != "" || model.errorMsg == "" || command != nil {
		t.Fatalf("rescan offline: outcome = %+v, error = %q, cmd = %v", model.outcome, model.errorMsg, command != nil)
	}
	// stop still fires offline.
	updated, _ = model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}, "x")
	model = updated.(appModel)
	if model.outcome.Action != ActStop || model.outcome.Connection != "corp" {
		t.Fatalf("stop offline: outcome = %+v", model.outcome)
	}
}

func TestRegisteredActionShowsProgressAndResultModal(t *testing.T) {
	handler := func(connection string) (ActionResult, error) {
		return ActionResult{Title: "Connection status", Body: connection + " is running"}, nil
	}
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{Actions: map[string]ActionHandler{ActStatus: handler}})
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}, "i")
	model = updated.(appModel)
	if command == nil || model.actionModal == nil || !model.actionModal.running || !strings.Contains(model.View(), "Working") {
		t.Fatalf("progress modal missing: modal = %+v, cmd nil = %v", model.actionModal, command == nil)
	}
	updated, followup := model.Update(command())
	model = updated.(appModel)
	if model.actionModal == nil || model.actionModal.running || !strings.Contains(model.View(), "corp is running") {
		t.Fatalf("result modal missing: modal = %+v", model.actionModal)
	}
	if followup == nil {
		t.Fatal("completed action did not schedule auto-dismiss")
	}
	updated, _ = model.Update(modalDismissTickMsg{id: model.actionModal.id})
	model = updated.(appModel)
	if model.actionModal != nil || model.outcome.Action != "" {
		t.Fatalf("auto-dismiss changed app state: modal = %+v, outcome = %+v", model.actionModal, model.outcome)
	}
}

func TestRegisteredActionErrorStaysInProgram(t *testing.T) {
	handler := func(string) (ActionResult, error) { return ActionResult{}, errors.New("status unavailable") }
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{Actions: map[string]ActionHandler{ActStatus: handler}})
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}, "i")
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if model.actionModal == nil || !model.actionModal.failed || !strings.Contains(model.View(), "status unavailable") {
		t.Fatalf("error modal missing: %+v", model.actionModal)
	}
}

func TestForgetRequiresConfirmationAndRemovesRowInUI(t *testing.T) {
	called := false
	model := newTestApp([]ConnSummary{{Name: "corp"}, {Name: "other"}}, RunOptions{
		Actions: map[string]ActionHandler{
			ActForget: func(name string) (ActionResult, error) {
				called = true
				return ActionResult{Title: "Connection forgotten", Body: name + " removed", RemoveConnection: true}, nil
			},
		},
	})
	updated, command := model.requestAction(ActForget, "corp", ViewHome)
	model = updated.(appModel)
	if command != nil || called || model.actionModal == nil || model.actionModal.confirmAction != ActForget {
		t.Fatal("forget must ask for confirmation before running")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model = updated.(appModel)
	if model.actionModal != nil || called {
		t.Fatal("canceling forget should not run the action")
	}
	updated, _ = model.requestAction(ActForget, "corp", ViewHome)
	model = updated.(appModel)
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if !called || len(model.options.Summaries) != 1 || model.options.Summaries[0].Name != "other" || model.outcome.Action != "" {
		t.Fatalf("forget did not update the in-app rows: called=%v summaries=%+v outcome=%+v", called, model.options.Summaries, model.outcome)
	}
}

func TestMutatingActionRefreshesConnectionInsideUI(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		Actions: map[string]ActionHandler{
			ActStop: func(string) (ActionResult, error) { return ActionResult{Title: "Stopped", Refresh: true}, nil },
		},
		RefreshConnection: func(name string) (ConnSummary, *ConnDetail, error) {
			return ConnSummary{Name: name, Running: true, PID: 222}, &ConnDetail{Name: name, Running: true, PID: 222}, nil
		},
	})
	updated, actionCommand := model.requestAction(ActStop, "corp", ViewHome)
	model = updated.(appModel)
	updated, refreshCommand := model.Update(actionCommand())
	model = updated.(appModel)
	if refreshCommand == nil {
		t.Fatal("mutating action did not request refreshed state")
	}
	updated, _ = model.Update(refreshCommand())
	model = updated.(appModel)
	if !model.options.Summaries[0].Running || model.options.Summaries[0].PID != 222 || model.outcome.Action != "" {
		t.Fatalf("refreshed state missing: %+v outcome=%+v", model.options.Summaries[0], model.outcome)
	}
}

func TestActionRefreshUpdatesHomeAfterModalDismissal(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp", Running: true, PID: 222}}, RunOptions{
		Actions: map[string]ActionHandler{
			ActStop: func(string) (ActionResult, error) { return ActionResult{Title: "Tunnel stopped", Refresh: true}, nil },
		},
		RefreshConnection: func(name string) (ConnSummary, *ConnDetail, error) {
			return ConnSummary{Name: name, Reachable: true}, &ConnDetail{Name: name}, nil
		},
	})
	updated, actionCommand := model.requestAction(ActStop, "corp", ViewHome)
	model = updated.(appModel)
	updated, refreshCommand := model.Update(actionCommand())
	model = updated.(appModel)
	if refreshCommand == nil || model.actionModal == nil || model.actionModal.running {
		t.Fatal("action did not complete and schedule refresh")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if model.actionModal != nil {
		t.Fatal("completed action modal was not dismissed")
	}
	updated, _ = model.Update(refreshCommand())
	model = updated.(appModel)
	if model.options.Summaries[0].Running || !model.options.Summaries[0].Reachable {
		t.Fatalf("late refresh was discarded: %+v", model.options.Summaries[0])
	}
}

func TestActionModalAutoDismissesAfterSuccess(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		Actions: map[string]ActionHandler{
			ActStart: func(string) (ActionResult, error) {
				return ActionResult{Title: "Tunnel started", Body: "corp is running."}, nil
			},
		},
	})
	updated, actionCommand := model.requestAction(ActStart, "corp", ViewHome)
	model = updated.(appModel)
	updated, _ = model.Update(actionCommand())
	model = updated.(appModel)
	if model.actionModal == nil || model.actionModal.running || model.actionModal.failed {
		t.Fatalf("action did not complete into a result modal: %+v", model.actionModal)
	}
	updated, _ = model.Update(modalDismissTickMsg{id: model.actionModal.id})
	model = updated.(appModel)
	if model.actionModal != nil {
		t.Fatal("completed action modal was not auto-dismissed")
	}
}

func TestActionModalAutoDismissIgnoresStaleTickAndFailures(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		Actions: map[string]ActionHandler{
			ActStart: func(string) (ActionResult, error) { return ActionResult{}, errors.New("boom") },
		},
	})
	updated, actionCommand := model.requestAction(ActStart, "corp", ViewHome)
	model = updated.(appModel)
	updated, _ = model.Update(actionCommand())
	model = updated.(appModel)
	if model.actionModal == nil || !model.actionModal.failed {
		t.Fatal("failure was not shown in the modal")
	}
	// Failures wait for explicit dismissal: the timer must not clear them.
	updated, _ = model.Update(modalDismissTickMsg{id: model.actionModal.id})
	model = updated.(appModel)
	if model.actionModal == nil {
		t.Fatal("failed action modal was auto-dismissed")
	}
	// A stale tick (different id, e.g. from a previous action) is ignored.
	updated, _ = model.Update(modalDismissTickMsg{id: model.actionModal.id + 999})
	model = updated.(appModel)
	if model.actionModal == nil {
		t.Fatal("stale tick dismissed an unrelated modal")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if model.actionModal != nil {
		t.Fatal("failed action modal should still be dismissible by key")
	}
}

func TestFailedStopStillRefreshesActualTunnelState(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp", Running: true, PID: 222}}, RunOptions{
		Actions: map[string]ActionHandler{
			ActStop: func(string) (ActionResult, error) {
				return ActionResult{Title: "Stop attempted", Refresh: true}, errors.New("stop timed out")
			},
		},
		RefreshConnection: func(name string) (ConnSummary, *ConnDetail, error) {
			return ConnSummary{Name: name, Reachable: true}, &ConnDetail{Name: name}, nil
		},
	})
	updated, actionCommand := model.requestAction(ActStop, "corp", ViewHome)
	model = updated.(appModel)
	updated, refreshCommand := model.Update(actionCommand())
	model = updated.(appModel)
	if refreshCommand == nil || model.actionModal == nil || !model.actionModal.failed {
		t.Fatal("stop error should remain visible while refreshing state")
	}
	updated, _ = model.Update(refreshCommand())
	model = updated.(appModel)
	if model.options.Summaries[0].Running || !model.options.Summaries[0].Reachable {
		t.Fatalf("failed stop did not refresh actual state: %+v", model.options.Summaries[0])
	}
}

func TestPeriodicStatusRefreshUpdatesReachability(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp", Account: "111", Unreachable: true}}, RunOptions{
		RefreshStatus: func() []ConnSummary {
			return []ConnSummary{{Name: "corp", Account: "111", Reachable: true}}
		},
	})
	updated, next := model.Update(statusRefreshedMsg{summaries: model.options.RefreshStatus()})
	model = updated.(appModel)
	if next == nil {
		t.Fatal("status refresh did not schedule the next probe")
	}
	summary := model.options.Summaries[0]
	if summary.Unreachable || !summary.Reachable || summary.StatusLabel() != "● Reachable" {
		t.Fatalf("status refresh did not update reachability: %+v", summary)
	}

	updated, _ = model.Update(statusRefreshedMsg{})
	model = updated.(appModel)
	if !model.options.Summaries[0].Reachable {
		t.Fatal("empty refresh result must not clear the last known status")
	}
}

func TestEditorShowsActiveFieldAndCaret(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	editor := newConnectionEditor(cfg, []string{"default", "dev"})
	editor.field = 4
	editor.input = editor.values[4]
	view := renderEditor(editor, 8, "")
	for _, expected := range []string{"EDIT CONNECTION · corp", "Field 5/19 · target", "▶ target", "▏"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("editor view missing %q:\n%s", expected, view)
		}
	}

	editor.field = 15
	view = renderEditor(editor, 8, "")
	if !strings.Contains(view, "Field 16/19 · dns domains") || !strings.Contains(view, "▶ dns domains") {
		t.Fatalf("active DNS field was not kept visible:\n%s", view)
	}
}

func TestEditorCyclesAWSProfiles(t *testing.T) {
	cfg := &config.Config{Name: "corp", AWSProfile: "default"}
	model := newTestApp(nil, RunOptions{AWSProfiles: []string{"default", "production"}})
	model.editor = newConnectionEditor(cfg, model.options.AWSProfiles)
	model.editor.field = len(model.editor.fields) - 1
	model.editor.input = model.editor.values[model.editor.field]
	updated, _ := model.updateEditor("right", nil)
	model = updated.(appModel)
	if model.editor.values[model.editor.field] != "production" {
		t.Fatalf("selected profile = %q, want production", model.editor.values[model.editor.field])
	}
	updated, _ = model.updateEditor("left", nil)
	model = updated.(appModel)
	if model.editor.values[model.editor.field] != "default" {
		t.Fatalf("selected profile = %q, want default", model.editor.values[model.editor.field])
	}
}

func TestEditorEnumFieldsUseSelectableLists(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", Region: "eu-central-1", SSHPort: 22}
	model := newTestApp(nil, RunOptions{AWSProfiles: []string{"default", "production"}, AWSRegions: []string{"eu-central-1", "eu-west-1"}})
	model.editor = newConnectionEditor(cfg, model.options.AWSProfiles, model.options.AWSRegions)
	model.editor.field = 3
	updated, _ := model.updateEditor("enter", nil)
	model = updated.(appModel)
	if len(model.editor.choices) != 2 {
		t.Fatalf("region options = %v", model.editor.choices)
	}
	updated, _ = model.updateEditor("down", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if model.editor.values[3] != "eu-west-1" {
		t.Fatalf("region selection = %q", model.editor.values[3])
	}
	model.editor.field = 1
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if len(model.editor.choices) != 2 || !strings.Contains(model.View(), "dedicated") {
		t.Fatalf("kind options did not open: choices=%v view=%q", model.editor.choices, model.View())
	}
	updated, _ = model.updateEditor("down", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if model.editor.values[1] != string(config.ProfileKindDedicated) || len(model.editor.choices) != 0 {
		t.Fatalf("kind selection = %q, choices=%v", model.editor.values[1], model.editor.choices)
	}

	model.editor.field = 2
	model.editor.input = model.editor.values[2]
	updated, _ = model.updateEditor("x", runesOf("x"))
	model = updated.(appModel)
	if model.editor.values[2] != "ssm" {
		t.Fatalf("enum field accepted free text: method=%q", model.editor.values[2])
	}

	model.editor.field = len(model.editor.fields) - 1
	model.editor.input = model.editor.values[model.editor.field]
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("down", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if model.editor.values[len(model.editor.values)-1] != "production" {
		t.Fatalf("AWS profile selection = %q", model.editor.values[len(model.editor.values)-1])
	}
}

func TestEditorRoutePickerTogglesAndPersistsSelection(t *testing.T) {
	cfg := &config.Config{
		Name: "corp", SSHPort: 22,
		Routes: []config.NetworkRoute{
			{CIDR: "10.0.0.0/16", Source: "pivot_vpc", Selected: true},
			{CIDR: "10.1.0.0/16", Source: "subnet_route"},
		},
	}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, []string{"default"})
	model.editor.field = 12
	updated, _ := model.updateEditor("enter", nil)
	model = updated.(appModel)
	if !model.editor.routePicker || !strings.Contains(model.View(), "10.1.0.0/16") {
		t.Fatalf("route picker did not open: %q", model.View())
	}
	updated, _ = model.updateEditor("down", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor(" ", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	draft, err := editorConfig(model.editor)
	if err != nil {
		t.Fatal(err)
	}
	if !draft.Routes[0].Selected || !draft.Routes[1].Selected {
		t.Fatalf("route selection was not persisted to draft: %+v", draft.Routes)
	}
	model.editor.routePicker = true
	model.editor.routeOriginal = append([]config.NetworkRoute(nil), model.editor.routes...)
	updated, _ = model.updateEditor("m", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("10.2.0.0/16", runesOf("10.2.0.0/16"))
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if model.editor.routePicker || len(model.editor.routes) != 3 || model.editor.routes[2].CIDR != "10.2.0.0/16" || !model.editor.routes[2].Selected {
		t.Fatalf("manual CIDR was not added: picker=%v routes=%+v", model.editor.routePicker, model.editor.routes)
	}
}

func TestEditorChecksSelectedAWSProfileInUI(t *testing.T) {
	cfg := &config.Config{Name: "corp", AWSProfile: "default", SSHPort: 22}
	checked := false
	model := newTestApp(nil, RunOptions{
		AWSProfiles: []string{"default", "production"},
		CheckAWSProfile: func(edited *config.Config, profile string) (ActionResult, error) {
			checked = true
			if edited.Name != "corp" || edited.AWSProfile != "production" || profile != "production" {
				t.Errorf("profile check config=%+v profile=%q", edited, profile)
			}
			return ActionResult{Title: "AWS profile readiness", Body: "account matches"}, nil
		},
	})
	model.editor = newConnectionEditor(cfg, model.options.AWSProfiles)
	model.editor.field = len(model.editor.fields) - 1
	model.editor.input = model.editor.values[model.editor.field]
	updated, _ := model.updateEditor("right", nil)
	model = updated.(appModel)
	updated, command := model.updateEditor("v", nil)
	model = updated.(appModel)
	if command == nil || model.actionModal == nil || !model.actionModal.running {
		t.Fatalf("profile check did not start in UI: modal=%+v command nil=%v", model.actionModal, command == nil)
	}
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if !checked || model.actionModal == nil || model.actionModal.running || !strings.Contains(model.View(), "account matches") {
		t.Fatalf("profile check result missing: checked=%v modal=%+v", checked, model.actionModal)
	}
}

func TestSavedAWSProfileRefreshesHomeAndConnectionViews(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", Region: "eu-central-1", Target: "i-1", SSHPort: 22, AWSProfile: "default"}
	saved := cfg
	model := newTestApp([]ConnSummary{{Name: "corp", AWSProfile: "default"}}, RunOptions{
		Level:       1,
		Detail:      &ConnDetail{Name: "corp", AWSProfile: "default"},
		AWSProfiles: []string{"default", "production"},
		LoadDetail: func(name string) (*ConnDetail, error) {
			return &ConnDetail{Name: name, AWSProfile: saved.AWSProfile}, nil
		},
		SaveConfig: func(_ string, edited *config.Config) error {
			saved = edited
			return nil
		},
	})
	model.editor = newConnectionEditor(cfg, model.options.AWSProfiles)
	model.editor.field = len(model.editor.fields) - 1
	model.editor.values[model.editor.field] = "production"
	model.editor.input = "production"
	updated, saveCommand := model.updateEditor("ctrl+s", nil)
	model = updated.(appModel)
	updated, reloadCommand := model.Update(saveCommand())
	model = updated.(appModel)
	if reloadCommand == nil || model.options.Summaries[0].AWSProfile != "production" {
		t.Fatalf("save did not refresh home summary: profile=%q reload=%v", model.options.Summaries[0].AWSProfile, reloadCommand != nil)
	}
	updated, _ = model.Update(reloadCommand())
	model = updated.(appModel)
	if model.options.Detail.AWSProfile != "production" {
		t.Fatalf("save did not refresh connection detail: profile=%q", model.options.Detail.AWSProfile)
	}
}

func TestExternalConfigChangeRefreshesHomeAndConnection(t *testing.T) {
	initial := &config.Config{Name: "corp", AccountID: "123456789012", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", Region: "eu-central-1", Target: "i-old", SSHPort: 22, AWSProfile: "default"}
	changed := *initial
	changed.Region = "eu-west-1"
	changed.Target = "i-new"
	changed.AWSProfile = "production"
	model := newTestApp([]ConnSummary{{Name: "corp", Account: initial.AccountID, Region: initial.Region, AWSProfile: initial.AWSProfile}}, RunOptions{
		Level:          1,
		Detail:         &ConnDetail{Name: "corp", Region: initial.Region, Target: initial.Target, AWSProfile: initial.AWSProfile},
		ConfigSnapshot: []*config.Config{initial},
		LoadDetail: func(name string) (*ConnDetail, error) {
			return &ConnDetail{Name: name, Region: changed.Region, Target: changed.Target, AWSProfile: changed.AWSProfile}, nil
		},
		RefreshConfigs: func() ([]*config.Config, error) { return []*config.Config{&changed}, nil },
	})
	model.knownConfigs = model.options.ConfigSnapshot
	reload := model.applyRefreshedConfigs([]*config.Config{&changed})
	if reload == nil || model.options.Summaries[0].Region != "eu-west-1" || model.options.Summaries[0].AWSProfile != "production" {
		t.Fatalf("home summary not refreshed: summary=%+v reload=%v", model.options.Summaries[0], reload != nil)
	}
	if model.loadingDetail != "corp" {
		t.Fatalf("detail reload not scheduled: loading=%q", model.loadingDetail)
	}
	updated, _ := model.Update(detailLoadedMsg{name: "corp", detail: &ConnDetail{Name: "corp", Region: "eu-west-1", Target: "i-new", AWSProfile: "production"}})
	model = updated.(appModel)
	if model.options.Detail.Region != "eu-west-1" || model.options.Detail.AWSProfile != "production" {
		t.Fatalf("connection detail not refreshed: %+v", model.options.Detail)
	}
}

func TestExternalConfigChangeBlocksStaleEditorSave(t *testing.T) {
	initial := &config.Config{Name: "corp", AccountID: "123456789012", SSHPort: 22, AWSProfile: "default"}
	changed := *initial
	changed.AWSProfile = "production"
	saved := false
	model := newTestApp([]ConnSummary{{Name: "corp", Account: initial.AccountID, AWSProfile: initial.AWSProfile}}, RunOptions{
		ConfigSnapshot: []*config.Config{initial},
		SaveConfig:     func(string, *config.Config) error { saved = true; return nil },
	})
	model.knownConfigs = model.options.ConfigSnapshot
	model.editor = newConnectionEditor(initial, []string{"default", "production"})
	model.applyRefreshedConfigs([]*config.Config{&changed})
	if model.editor == nil || !model.editor.conflicted {
		t.Fatal("editor should be marked conflicted after an external change")
	}
	updated, command := model.updateEditor("ctrl+s", nil)
	model = updated.(appModel)
	if saved || command != nil || !strings.Contains(model.errorMsg, "changed on disk") {
		t.Fatalf("stale save was not blocked: saved=%v error=%q", saved, model.errorMsg)
	}
}

func TestEditAndDNSShortcutLabels(t *testing.T) {
	home := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{}).homeChrome()
	connection := appModel{options: RunOptions{Level: 1, Detail: &ConnDetail{Name: "corp"}}}.connectionChrome()
	labels := func(hints []Hint) map[string]string {
		result := make(map[string]string, len(hints))
		for _, hint := range hints {
			result[hint.Key] = hint.Label
		}
		return result
	}
	if labels(home.Hints)["e"] != "edit" || labels(home.Hints)["d"] != "dns" || labels(home.Hints)["v"] != "check AWS" {
		t.Fatalf("home edit/DNS hints = %v", labels(home.Hints))
	}
	if labels(connection.Hints)["e"] != "edit" || labels(connection.Hints)["d"] != "dns" || labels(connection.Hints)["v"] != "check AWS" {
		t.Fatalf("connection edit/DNS hints = %v", labels(connection.Hints))
	}
}

func TestAWSCheckShortcutRunsInBothPanels(t *testing.T) {
	handler := func(name string) (ActionResult, error) {
		return ActionResult{Title: "AWS profile readiness", Body: name + " profile checked"}, nil
	}
	for _, connectionLevel := range []bool{false, true} {
		options := RunOptions{Actions: map[string]ActionHandler{ActAWSCheck: handler}}
		if connectionLevel {
			options.Level = 1
			options.Detail = &ConnDetail{Name: "corp"}
		}
		model := newTestApp([]ConnSummary{{Name: "corp"}}, options)
		var updated tea.Model
		var command tea.Cmd
		if connectionLevel {
			updated, command = model.updateConnectionLevel(nil, "v")
		} else {
			updated, command = model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")}, "v")
		}
		model = updated.(appModel)
		if command == nil {
			t.Fatalf("connectionLevel=%v: check did not start", connectionLevel)
		}
		updated, _ = model.Update(command())
		model = updated.(appModel)
		if model.actionModal == nil || model.actionModal.running || !strings.Contains(model.View(), "corp profile checked") {
			t.Fatalf("connectionLevel=%v: check result missing: %+v", connectionLevel, model.actionModal)
		}
	}
}

func TestRescanPickerAndSaveStayInsideUI(t *testing.T) {
	cfg := &config.Config{Name: "corp", AccountID: "123456789012", InstanceID: "i-old", Target: "i-old", SSHPort: 22}
	saved := cfg
	var selected RescanCandidate
	model := newTestApp([]ConnSummary{{Name: "corp", Account: cfg.AccountID, Unreachable: true}}, RunOptions{
		Level:  1,
		Detail: &ConnDetail{Name: "corp", Target: "i-old"},
		PrepareRescan: func(string) ([]RescanCandidate, error) {
			return []RescanCandidate{
				{Name: "new pivot", InstanceID: "i-new", Method: "ssm", Target: "i-new", Reachable: true},
				{Name: "old pivot", InstanceID: "i-old", Method: "ssm", Target: "i-old", Reachable: true, Current: true},
			}, nil
		},
		SaveRescan: func(_ string, candidate RescanCandidate) (*config.Config, error) {
			selected = candidate
			updated := *saved
			updated.InstanceID, updated.Target = candidate.InstanceID, candidate.Target
			saved = &updated
			return &updated, nil
		},
		LoadDetail: func(string) (*ConnDetail, error) {
			return &ConnDetail{Name: "corp", Target: saved.Target}, nil
		},
	})
	updated, scanCommand := model.requestAction(ActRescan, "corp", ViewConnection)
	model = updated.(appModel)
	updated, _ = model.Update(scanCommand())
	model = updated.(appModel)
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(appModel)
	if model.rescan == nil || model.outcome.Action != "" || !strings.Contains(model.View(), "i-new") {
		t.Fatalf("rescan picker left the UI or did not render: selector=%+v outcome=%+v view=%q", model.rescan, model.outcome, model.View())
	}
	updated, saveCommand := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if saveCommand == nil || model.actionModal == nil || !model.actionModal.running {
		t.Fatal("rescan save did not start in the UI")
	}
	updated, reloadCommand := model.Update(saveCommand())
	model = updated.(appModel)
	if selected.InstanceID != "i-new" || reloadCommand == nil || model.options.Summaries[0].Name != "corp" || !model.options.Summaries[0].Reachable || model.options.Summaries[0].Unreachable || model.options.Summaries[0].StatusLabel() != "● Reachable" {
		t.Fatalf("rescan save result: selected=%+v reload=%v", selected, reloadCommand != nil)
	}
	updated, _ = model.Update(reloadCommand())
	model = updated.(appModel)
	if model.options.Detail.Target != "i-new" || model.outcome.Action != "" {
		t.Fatalf("rescan did not refresh detail inside UI: detail=%+v outcome=%+v", model.options.Detail, model.outcome)
	}
}

func TestEnterOpensConnectionView(t *testing.T) {
	loader := func(name string) (*ConnDetail, error) {
		return &ConnDetail{Name: name, Method: "ssh", Target: "i-1", TargetOK: true}, nil
	}
	model := newTestApp([]ConnSummary{{Name: "corp"}, {Name: "dns-split"}}, RunOptions{LoadDetail: loader})

	updated, _ := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyDown}, "down")
	model = updated.(appModel)
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyEnter}, "enter")
	model = updated.(appModel)
	if command == nil {
		t.Fatal("enter must load connection details")
	}
	if model.loadingDetail != "dns-split" {
		t.Fatalf("loading detail = %q", model.loadingDetail)
	}
	updated, followup := model.Update(command())
	model = updated.(appModel)
	if followup != nil || model.level() != 1 || model.options.Detail.Name != "dns-split" {
		t.Fatalf("detail load: level = %d, detail = %+v, cmd = %v", model.level(), model.options.Detail, followup != nil)
	}
	if model.outcome.Action != "" || model.outcome.Connection != "dns-split" || model.outcome.View != ViewConnection {
		t.Fatalf("outcome = %+v", model.outcome)
	}
}

func TestEscFromConnectionReturnsHome(t *testing.T) {
	detail := &ConnDetail{Name: "corp", Method: "ssh", Target: "i-1", TargetOK: true}
	model := appModel{options: RunOptions{Level: 1, Detail: detail}, home: newHomeTable(nil, 0)}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.level() != 0 || model.outcome.Action != "" || model.outcome.Connection != "corp" || model.outcome.View != ViewHome {
		t.Fatalf("outcome = %+v", model.outcome)
	}
}

func TestDetailCardShowsTunnelTargetDNSRoutes(t *testing.T) {
	detail := &ConnDetail{
		Name: "dns-split", Kind: "vpc", Method: "ssm", Region: "eu-central-1", AWSProfile: "bastionctl-test-profile-one", SSHPort: 22,
		Running: true, PID: 2431, Target: "i-00de", TargetOK: true,
		DNSSummary:      "split (1 domain(s) via 172.31.0.2)",
		DNSWantsInstall: true,
		Routes: []config.NetworkRoute{
			{CIDR: "10.0.0.0/16", Selected: true, Source: "pivot_vpc"},
		},
	}
	card := renderDetailCard(detail, 100)
	for _, expected := range []string{"TUNNEL", "running · PID 2431", "AWS", "bastionctl-test-profile-one", "TARGET", "i-00de", "DNS", "resolver config missing", "ROUTES", "10.0.0.0/16"} {
		if !strings.Contains(card, expected) {
			t.Fatalf("detail card missing %q:\n%s", expected, card)
		}
	}
}

func TestDetailCardRespectsBudget(t *testing.T) {
	detail := &ConnDetail{
		Name: "corp", Method: "ssh", Target: "i-1", TargetOK: true,
		Routes: []config.NetworkRoute{
			{CIDR: "10.0.0.0/16", Selected: true},
			{CIDR: "10.1.0.0/16", Selected: true},
			{CIDR: "10.2.0.0/16", Selected: true},
		},
	}
	card := renderDetailCard(detail, 3)
	if lines := strings.Count(card, "\n") + 1; lines > 3 {
		t.Fatalf("card exceeded budget %d: %d lines:\n%s", 3, lines, card)
	}
}

func TestConnectionFooterIsAtTerminalBottom(t *testing.T) {
	detail := &ConnDetail{Name: "corp", Method: "ssh", Target: "i-1", TargetOK: true}
	model := appModel{options: RunOptions{Level: 1, Detail: detail}, home: newHomeTable(nil, 0)}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	view := updated.(appModel).View()
	if lines := strings.Count(view, "\n") + 1; lines != 20 {
		t.Fatalf("connection view has %d lines, want terminal height 20:\n%s", lines, view)
	}
	if !strings.Contains(strings.Split(view, "\n")[19], "esc") {
		t.Fatalf("footer is not on final terminal row:\n%s", view)
	}
}

func TestLogsStreamInsideUIAndCanClose(t *testing.T) {
	lines := make(chan string, 2)
	lines <- "first log line"
	lines <- "second log line"
	close(lines)
	done := make(chan error, 1)
	done <- nil
	close(done)
	cancelled := false
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		StartLogs: func(string) (*LogSession, error) {
			return &LogSession{Lines: lines, Done: done, Cancel: func() { cancelled = true }}, nil
		},
	})
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")}, "l")
	model = updated.(appModel)
	updated, command = model.Update(command())
	model = updated.(appModel)
	if model.logSession == nil || model.logDone || !strings.Contains(model.View(), "first log line") {
		t.Fatalf("log view not streaming: done=%v session=%v view=%q", model.logDone, model.logSession != nil, model.View())
	}
	for command != nil {
		updated, command = model.Update(command())
		model = updated.(appModel)
	}
	if !model.logDone || !strings.Contains(model.View(), "second log line") {
		t.Fatalf("log stream did not finish: done=%v view=%q", model.logDone, model.View())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.logSession != nil || !cancelled || model.outcome.Action != "" {
		t.Fatalf("log close: session=%v cancelled=%v outcome=%+v", model.logSession != nil, cancelled, model.outcome)
	}
}

func TestLaunchRunsInCapturedOutputView(t *testing.T) {
	lines := make(chan string, 1)
	lines <- "sshuttle output"
	close(lines)
	done := make(chan error, 1)
	done <- nil
	close(done)
	canceled := false
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		StartLaunch: func(string) (*LogSession, error) {
			return &LogSession{Lines: lines, Done: done, Cancel: func() { canceled = true }}, nil
		},
	})
	updated, command := model.requestAction(ActLaunch, "corp", ViewHome)
	model = updated.(appModel)
	for command != nil {
		updated, command = model.Update(command())
		model = updated.(appModel)
	}
	if model.logSession == nil || !strings.Contains(model.View(), "Foreground tunnel output") || !strings.Contains(model.View(), "sshuttle output") || model.outcome.Action != "" {
		t.Fatalf("launch did not stay in captured UI view: view=%q outcome=%+v", model.View(), model.outcome)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.logSession != nil || !canceled {
		t.Fatalf("launch close did not cancel its process: session=%v canceled=%v", model.logSession != nil, canceled)
	}
}

func TestNewConnectionWizardStaysInsideUI(t *testing.T) {
	var saved *config.Config
	model := newTestApp(nil, RunOptions{
		AWSProfiles: []string{"default"},
		StartNew: func() (NewConnectionStart, error) {
			return NewConnectionStart{Regions: []string{"eu-central-1"}, AccountID: "123456789012", SSHPort: 22}, nil
		},
		ScanNew: func(region, kind string) ([]RescanCandidate, error) {
			if region != "eu-central-1" || kind != string(config.ProfileKindVPC) {
				t.Errorf("scan arguments = %q, %q", region, kind)
			}
			return []RescanCandidate{{Name: "pivot", InstanceID: "i-new", Method: "ssm", Target: "i-new", VpcID: "vpc-1", VpcCIDRs: []string{"10.0.0.0/16"}, Reachable: true}}, nil
		},
		PrepareNewRoutes: func(_, _ string, candidate RescanCandidate) ([]config.NetworkRoute, error) {
			return []config.NetworkRoute{{CIDR: candidate.VpcCIDRs[0], Source: "pivot_vpc", Selected: true}}, nil
		},
		SaveNewConfig: func(cfg *config.Config) error {
			saved = cfg
			return nil
		},
		LoadDetail: func(name string) (*ConnDetail, error) {
			return &ConnDetail{Name: name, Target: saved.Target, Method: saved.ConnectionMethod}, nil
		},
	})
	updated, command := model.requestAction(ActNew, "", ViewHome)
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if model.newWizard == nil || model.outcome.Action != "" {
		t.Fatalf("New did not open the in-app wizard: wizard=%+v outcome=%+v", model.newWizard, model.outcome)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if model.newWizard == nil || model.newWizard.step != "candidate" {
		t.Fatalf("candidate stage missing: %+v", model.newWizard)
	}
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if model.newWizard == nil || model.newWizard.step != "routes" {
		t.Fatalf("route stage missing: %+v", model.newWizard)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if model.editor == nil || !model.editor.isNew || model.editor.values[10] != "i-new" {
		t.Fatalf("new connection editor missing: %+v", model.editor)
	}
	updated, _ = model.updateEditor("corp-new", runesOf("corp-new"))
	model = updated.(appModel)
	updated, command = model.updateEditor("ctrl+s", nil)
	model = updated.(appModel)
	updated, reload := model.Update(command())
	model = updated.(appModel)
	if saved == nil || saved.Name != "corp-new" || reload == nil || len(model.options.Summaries) != 1 {
		t.Fatalf("new config was not saved/refreshed: saved=%+v reload=%v summaries=%+v", saved, reload != nil, model.options.Summaries)
	}
	updated, _ = model.Update(reload())
	model = updated.(appModel)
	if model.options.Detail == nil || model.options.Detail.Target != "i-new" || model.outcome.Action != "" {
		t.Fatalf("new connection detail not opened in-app: detail=%+v outcome=%+v", model.options.Detail, model.outcome)
	}
}
