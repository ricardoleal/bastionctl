package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"bastionctl/internal/project"
	tea "bastionctl/internal/ui/tea"

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

func TestSearchKeepsDashboardRowsWhenNoMatches(t *testing.T) {
	model := dashboardTestModel()
	model.options.Account = ""
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(appModel)
	before := model.dashboardMetaRows()
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("no-such-connection")})
	model = updated.(appModel)
	after := model.dashboardMetaRows()
	if len(after) != len(before) {
		t.Fatalf("dashboard rows changed from %v to %v", before, after)
	}
	for index := range before {
		if before[index].Label != after[index].Label {
			t.Fatalf("dashboard row %d changed: %v to %v", index, before[index], after[index])
		}
	}
	view := model.View()
	for _, text := range []string{"Method", "DNS", "Bastionctl", "v1.2.3", "connections(no-such-connection)[0]"} {
		if !strings.Contains(view, text) {
			t.Fatalf("filtered view missing %q:\n%s", text, view)
		}
	}
}

func TestEscapeClearsScopedSearch(t *testing.T) {
	detail := &ConnDetail{Name: "corp", Target: "i-123", TargetOK: true}
	model := newTestApp(nil, RunOptions{Level: 1, Detail: detail})
	model.searchFilters = map[string]string{searchDetail: "target"}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("-missing")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.searching || model.searchFilters[searchDetail] != "" || model.level() != 1 || !strings.Contains(model.View(), "DNS") {
		t.Fatalf("escape did not clear scoped search: %q", model.View())
	}
	model.searchFilters[searchDetail] = "target"
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.searchFilters[searchDetail] != "" || model.level() != 1 || !strings.Contains(model.View(), "DNS") {
		t.Fatalf("escape did not clear applied detail filter: %q", model.View())
	}
}

func TestEscapeClearsActiveAndCommittedHomeFilter(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}, {Name: "analytics"}}, RunOptions{})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)
	press := func(message tea.KeyMsg) {
		t.Helper()
		updated, _ := model.Update(message)
		model = updated.(appModel)
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("missing")})
	press(tea.KeyMsg{Type: tea.KeyEsc})
	if model.searching || model.searchFilter != "" || len(model.home.Rows()) != 2 || !strings.Contains(model.View(), "corp") {
		t.Fatalf("escape did not clear active filter: %q", model.View())
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("analytics")})
	press(tea.KeyMsg{Type: tea.KeyEnter})
	press(tea.KeyMsg{Type: tea.KeyEsc})
	if model.searchFilter != "" || len(model.home.Rows()) != 2 || model.currentName() != "analytics" {
		t.Fatalf("escape did not clear committed filter or preserve selection: %q", model.View())
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
	_, detailView, found := strings.Cut(view, "connection(corp)")
	if !found || !strings.Contains(detailView, "i-123") || strings.Contains(detailView, "DNS") || model.searchFilters[searchDetail] != "target" {
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
	found := false
	for _, hint := range model.chrome().Hints {
		if hint.Key == "a" && hint.Label == "launch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("home hints missing launch action: %v", model.chrome().Hints)
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
	if !strings.Contains(view, "|") {
		t.Fatalf("empty input bar missing from view:\n%s", view)
	}
	if !strings.Contains(ansiEscapePattern.ReplaceAllString(view, ""), ":|start") {
		t.Fatalf("inline suggestion missing:\n%s", view)
	}
	// Typed characters show up immediately.
	updated, _ = model.updateShell("stop", runesOf("stop"))
	model = updated.(appModel)
	if view := model.View(); !strings.Contains(view, "stop") || !strings.Contains(view, "|") {
		t.Fatalf("typed input missing:\n%s", view)
	}
}

func TestShellInlineCompletion(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)
	model.shell = true
	model.shellInput = "st"
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	if !strings.Contains(view, ":st|art") || model.shellInput != "st" {
		t.Fatalf("inline suggestion changed input or is missing:\n%s", view)
	}
	updated, _ = model.updateShell("tab", nil)
	model = updated.(appModel)
	if model.shellInput != "start " || !strings.Contains(ansiEscapePattern.ReplaceAllString(model.View(), ""), ":start |corp") {
		t.Fatalf("tab did not accept suggested command: input=%q view=%q", model.shellInput, model.View())
	}
	updated, _ = model.updateShell("tab", nil)
	model = updated.(appModel)
	if model.shellInput != "start corp " {
		t.Fatalf("tab did not complete connection: %q", model.shellInput)
	}
	model.shellInput = "dns i"
	if !strings.Contains(ansiEscapePattern.ReplaceAllString(model.View(), ""), ":dns i|nstall") {
		t.Fatalf("DNS completion missing: %q", model.View())
	}
	model.shellInput = "unmatched"
	updated, _ = model.updateShell("tab", nil)
	model = updated.(appModel)
	if model.shellInput != "unmatched" || !strings.Contains(ansiEscapePattern.ReplaceAllString(model.View(), ""), ":unmatched|") {
		t.Fatalf("unmatched input changed: %q", model.shellInput)
	}
}

func TestShellCompletesDNSAndUnicodeConnections(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "café-edge"}}, RunOptions{})
	model.shell = true
	model.shellInput = "dns i"
	updated, _ := model.updateShell("tab", nil)
	model = updated.(appModel)
	if model.shellInput != "dns install " {
		t.Fatalf("DNS action completion = %q", model.shellInput)
	}
	updated, _ = model.updateShell("tab", nil)
	model = updated.(appModel)
	if model.shellInput != "dns install café-edge " {
		t.Fatalf("DNS connection completion = %q", model.shellInput)
	}
	model.shellInput = "logs café"
	updated, _ = model.updateShell("backspace", nil)
	model = updated.(appModel)
	if model.shellInput != "logs caf" {
		t.Fatalf("Unicode backspace = %q", model.shellInput)
	}
	updated, _ = model.updateShell("tab", nil)
	model = updated.(appModel)
	if model.shellInput != "logs café-edge " {
		t.Fatalf("Unicode connection completion = %q", model.shellInput)
	}
}

func TestShellAutocompleteUsesOnePromptRow(t *testing.T) {
	base := Chrome{Hints: []Hint{{"q", "quit"}}}
	command := base
	command.ShellOpen = true
	command.Shell = "st"
	command.ShellSuggestions = []string{"start", "stop"}
	if got := base.Budget(20, 80) - command.Budget(20, 80); got != 1 {
		t.Fatalf("command prompt uses %d extra rows, want 1", got)
	}
	view := ansiEscapePattern.ReplaceAllString(command.View("┌─ connections(all)[1] ─┐", 80, 20), "")
	rows := strings.Split(view, "\n")
	for index, row := range rows {
		if strings.Contains(row, "connections(all)") {
			if index == 0 || !strings.Contains(rows[index-1], ":st|art") {
				t.Fatalf("inline prompt not immediately above panel:\n%s", view)
			}
			return
		}
	}
	t.Fatalf("panel missing:\n%s", view)
}

func TestShellAutocompleteFitsNarrowWidths(t *testing.T) {
	for _, width := range []int{1, 2, 8, 20} {
		prompt := renderCommandPrompt(":", "st", []string{"start-界界界"}, width)
		if cells := lipgloss.Width(prompt); cells > width {
			t.Fatalf("width %d: autocomplete uses %d cells: %q", width, cells, prompt)
		}
	}
}

func TestChromePromptsPrecedePanel(t *testing.T) {
	for _, chrome := range []Chrome{
		{Hints: []Hint{{"q", "quit"}}, SearchOpen: true, Search: "corp"},
		{Hints: []Hint{{"q", "quit"}}, ShellOpen: true, Shell: "stop"},
	} {
		view := ansiEscapePattern.ReplaceAllString(chrome.View("┌─ connections(all)[4] ─┐", 80, 10), "")
		lines := strings.Split(view, "\n")
		panel := -1
		for index, line := range lines {
			if strings.Contains(line, "connections(all)[4]") {
				panel = index
			}
		}
		if panel < 1 || !(strings.Contains(lines[panel-1], "/corp") || strings.Contains(lines[panel-1], ":stop")) {
			t.Fatalf("prompt not immediately above panel: %q", view)
		}
		if len(lines) != 10 {
			t.Fatalf("got %d lines, want 10: %q", len(lines), view)
		}
	}
}

func TestChromeAlignsDashboardAndShortcuts(t *testing.T) {
	chrome := Chrome{
		Dashboard: []string{"Profile    default", "Selected   corp", "Version    dev"},
		Hints:     []Hint{{"enter", "open"}, {"n", "new"}, {"s", "start"}, {"q", "quit"}},
	}
	rows := chrome.headerRows(120, 24)
	if len(rows) != 3 || !strings.Contains(rows[0], "Profile") || !strings.Contains(rows[0], "<enter>") || !strings.Contains(rows[0], "BASTIONCTL") {
		t.Fatalf("header columns not aligned: %q", rows)
	}
	if got := chrome.Budget(24, 120); got != 20 {
		t.Fatalf("body budget = %d, want 20", got)
	}
}

func TestChromeShortcutsUseExtraWidth(t *testing.T) {
	chrome := Chrome{Info: "ready", Hints: []Hint{{"a", "one"}, {"b", "two"}, {"c", "three"}, {"q", "quit"}}}
	spacing := func(width int) int {
		row := ansiEscapePattern.ReplaceAllString(chrome.headerRows(width, 24)[0], "")
		beforeFirst, _, firstFound := strings.Cut(row, "<a>")
		beforeSecond, _, secondFound := strings.Cut(row, "<b>")
		if !firstFound || !secondFound {
			t.Fatalf("shortcuts missing at width %d: %q", width, row)
		}
		return lipgloss.Width(beforeSecond) - lipgloss.Width(beforeFirst)
	}
	if spacing(160) <= spacing(120) {
		t.Fatal("shortcuts did not spread across additional width")
	}
}

func TestChromeLogoKeepsRightMargin(t *testing.T) {
	chrome := Chrome{Info: "ready", Hints: []Hint{{"q", "quit"}}}
	for _, width := range []int{80, 100, 120, 160} {
		row := ansiEscapePattern.ReplaceAllString(chrome.headerRows(width, 24)[0], "")
		if !strings.Contains(row, "BASTIONCTL") || !strings.HasSuffix(row, "  ") || lipgloss.Width(row) > width {
			t.Fatalf("width %d: logo clipped or missing margin: %q", width, row)
		}
	}
}

func TestChromeProjectFooterFits(t *testing.T) {
	t.Setenv("BASTIONCTL_NO_ICONS", "")
	t.Setenv("TERM_PROGRAM", "")
	for _, size := range [][2]int{{40, 12}, {80, 18}, {120, 24}} {
		chrome := Chrome{Info: "ready", Hints: []Hint{{"q", "quit"}}}
		view := chrome.View("panel", size[0], size[1])
		rows := strings.Split(view, "\n")
		footer := ansiEscapePattern.ReplaceAllString(rows[len(rows)-1], "")
		if len(rows) != size[1] || !strings.HasSuffix(footer, "\uf408 "+project.Repository) || lipgloss.Width(footer) != size[0] {
			t.Fatalf("size %v: invalid footer %q in %d rows", size, footer, len(rows))
		}
	}
}

func TestChromeProjectFooterCompactFallback(t *testing.T) {
	t.Setenv("BASTIONCTL_NO_ICONS", "")
	t.Setenv("TERM_PROGRAM", "")
	for _, test := range []struct {
		width int
		want  string
	}{{1, "\uf408"}, {12, "\uf408 bastionctl"}, {20, "\uf408 bastionctl"}} {
		view := (Chrome{}).View("panel", test.width, 8)
		rows := strings.Split(view, "\n")
		footer := ansiEscapePattern.ReplaceAllString(rows[len(rows)-1], "")
		if strings.TrimSpace(footer) != test.want || len(rows) != 8 {
			t.Fatalf("width %d: footer %q in %d rows", test.width, footer, len(rows))
		}
	}
}

func TestChromeProjectFooterPlainTextFallback(t *testing.T) {
	t.Setenv("BASTIONCTL_NO_ICONS", "1")
	for _, test := range []struct {
		width int
		want  string
	}{{1, "g"}, {12, "gh"}, {20, "gh bastionctl"}, {80, "gh " + project.Repository}} {
		view := (Chrome{}).View("panel", test.width, 8)
		rows := strings.Split(view, "\n")
		footer := ansiEscapePattern.ReplaceAllString(rows[len(rows)-1], "")
		if strings.TrimSpace(footer) != test.want || lipgloss.Width(footer) > test.width {
			t.Fatalf("width %d: unexpected plain-text footer %q", test.width, footer)
		}
	}
}

func TestChromeProjectFooterITermFallback(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("BASTIONCTL_NO_ICONS", "")
	t.Setenv("BASTIONCTL_ICONS", "")
	footer := func() string {
		view := (Chrome{}).View("panel", 80, 8)
		rows := strings.Split(view, "\n")
		return ansiEscapePattern.ReplaceAllString(rows[len(rows)-1], "")
	}
	if got := footer(); !strings.HasSuffix(got, "gh "+project.Repository) {
		t.Fatalf("iTerm footer did not use plain text: %q", got)
	}
	t.Setenv("BASTIONCTL_ICONS", "1")
	if got := footer(); !strings.HasSuffix(got, "\uf408 "+project.Repository) {
		t.Fatalf("iTerm opt-in did not enable the icon: %q", got)
	}
	t.Setenv("BASTIONCTL_NO_ICONS", "1")
	if got := footer(); !strings.HasSuffix(got, "gh "+project.Repository) {
		t.Fatalf("explicit plain-text fallback did not take priority: %q", got)
	}
}

func TestProjectFooterReachesTerminalRenderer(t *testing.T) {
	for _, test := range []struct {
		name        string
		noIcons     string
		icons       string
		termProgram string
		mark        string
	}{
		{name: "octocat", mark: "\uf408"},
		{name: "plain", noIcons: "1", mark: "gh"},
		{name: "iterm", termProgram: "iTerm.app", mark: "gh"},
		{name: "iterm with font", termProgram: "iTerm.app", icons: "1", mark: "\uf408"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BASTIONCTL_NO_ICONS", test.noIcons)
			t.Setenv("BASTIONCTL_ICONS", test.icons)
			t.Setenv("TERM_PROGRAM", test.termProgram)
			screen := tcell.NewSimulationScreen("UTF-8")
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(80, 8)
			view := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
			view.SetRect(0, 0, 80, 8)
			view.SetText(tview.TranslateANSI(tview.Escape((Chrome{}).View("panel", 80, 8))))
			view.Draw(screen)
			column := 80 - lipgloss.Width(test.mark+" "+project.Repository)
			character, _, _, _ := screen.GetContent(column, 7)
			if character != []rune(test.mark)[0] {
				t.Fatalf("footer glyph = %q, want %q", character, test.mark)
			}
		})
	}
}

func TestCaptureSenderLivesUntilRunAppCleanup(t *testing.T) {
	var captured func(Message)
	send := func(Message) {}
	cleanup := registerCaptureSender(func(next func(Message)) { captured = next }, send)
	if captured == nil {
		t.Fatal("sender not registered")
	}
	cleanup()
	if captured != nil {
		t.Fatal("sender not cleared during RunApp cleanup")
	}
}

func TestProjectFooterAppearsAcrossViews(t *testing.T) {
	t.Setenv("BASTIONCTL_NO_ICONS", "")
	t.Setenv("TERM_PROGRAM", "")
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)
	views := []struct {
		name  string
		apply func(*appModel)
	}{
		{"home", func(*appModel) {}},
		{"connection", func(m *appModel) { m.options.Level = 1; m.options.Detail = &ConnDetail{Name: "corp"} }},
		{"editor", func(m *appModel) { m.editor = newConnectionEditor(&config.Config{Name: "corp", SSHPort: 22}, nil) }},
		{"yaml", func(m *appModel) {
			m.editor = newConnectionEditor(&config.Config{Name: "corp", SSHPort: 22}, nil)
			if err := enterYAMLEditor(m.editor); err != nil {
				t.Fatal(err)
			}
		}},
		{"rescan", func(m *appModel) { m.rescan = &rescanSelector{connection: "corp"} }},
		{"wizard", func(m *appModel) { m.newWizard = &newConnectionWizard{step: "kind"} }},
		{"logs", func(m *appModel) { m.logSession = &LogSession{} }},
		{"help", func(m *appModel) { m.help = true }},
		{"command", func(m *appModel) { m.shell = true }},
		{"search", func(m *appModel) { m.beginSearch() }},
		{"notice", func(m *appModel) { m.setError("notice") }},
		{"action", func(m *appModel) { m.actionModal = &actionModal{title: "Action failed", body: "error"} }},
		{"password", func(m *appModel) { m.sudoPrompt = &sudoPasswordPrompt{} }},
	}
	for _, view := range views {
		current := model
		view.apply(&current)
		rows := strings.Split(current.View(), "\n")
		footer := ansiEscapePattern.ReplaceAllString(rows[len(rows)-1], "")
		if len(rows) != current.height || !strings.HasSuffix(footer, "\uf408 "+project.Repository) {
			t.Fatalf("%s: project footer missing or frame height changed: %q", view.name, rows[len(rows)-1])
		}
	}
}

func TestChromeLogoUsesCompactHeaderSpace(t *testing.T) {
	chrome := Chrome{Info: "ready", Hints: []Hint{{"q", "quit"}}}
	for _, width := range []int{120, 160} {
		rows := chrome.headerRows(width, 24)
		if len(rows) < 3 {
			t.Fatalf("width %d: compact logo missing rows: %q", width, rows)
		}
		for index, expected := range []string{"BASTIONCTL", "╔═╦═╗", "╚═╬═╝"} {
			plain := ansiEscapePattern.ReplaceAllString(rows[index], "")
			if !strings.Contains(plain, expected) || lipgloss.Width(plain) > width {
				t.Fatalf("width %d: logo row %d missing %q: %q", width, index, expected, plain)
			}
		}
	}
}

func TestChromeLogoFillsAvailableHeaderHeight(t *testing.T) {
	model := dashboardTestModel()
	for _, width := range []int{80} {
		updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		model = updated.(appModel)
		rows := model.chrome().headerRows(width, 30)
		if len(rows) < 7 {
			t.Fatalf("width %d: expected room for a tall logo, got %d rows", width, len(rows))
		}
		for index, expected := range []string{"BASTIONCTL", "╔╦═╦═╦╗", "║     ║", "║ ╔═╗ ║", "║ ║ ║ ║", "╚═╝ ╚═╝", "║"} {
			plain := ansiEscapePattern.ReplaceAllString(rows[index], "")
			if !strings.Contains(plain, expected) || lipgloss.Width(plain) > width {
				t.Fatalf("width %d: logo row %d missing %q: %q", width, index, expected, plain)
			}
		}
	}
}

func TestChromeLargeLogoUsesHeaderSpace(t *testing.T) {
	model := dashboardTestModel()
	for _, width := range []int{110, 120, 150, 160, 200} {
		updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		model = updated.(appModel)
		rows := model.chrome().headerRows(width, 30)
		for index, expected := range []string{"BASTIONCTL", "╔═╗ ╔═╗ ╔═╗ ╔═╗", "║ ╚═╝ ╚═╝ ╚═╝ ║", "║    ╔═══╗    ║", "║    ║ ╬ ║    ║", "╚════╣ ║ ╠════╝", "╚═╩═╝"} {
			plain := ansiEscapePattern.ReplaceAllString(rows[index], "")
			before, _, found := strings.Cut(plain, expected)
			if !found || lipgloss.Width(before) < width-30 || lipgloss.Width(plain) > width {
				t.Fatalf("width %d: large logo row %d misplaced: %q", width, index, plain)
			}
		}
	}
}

func TestChromeLargeLogoWallsAlign(t *testing.T) {
	model := dashboardTestModel()
	for _, width := range []int{110, 120, 160, 200} {
		updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		model = updated.(appModel)
		rows := model.chrome().headerRows(width, 30)
		left, right := -1, -1
		for index, ends := range [][2]string{{"╔", "╗"}, {"║", "║"}, {"║", "║"}, {"║", "║"}, {"╚", "╝"}} {
			line := ansiEscapePattern.ReplaceAllString(rows[index+1], "")
			first, last := strings.Index(line, ends[0]), strings.LastIndex(line, ends[1])
			if first < 0 || last < 0 {
				t.Fatalf("width %d: missing wall on row %d: %q", width, index+1, line)
			}
			firstColumn, lastColumn := lipgloss.Width(line[:first]), lipgloss.Width(line[:last])
			if index == 0 {
				left, right = firstColumn, lastColumn
			} else if firstColumn != left || lastColumn != right {
				t.Fatalf("width %d: row %d walls at %d/%d, want %d/%d", width, index+1, firstColumn, lastColumn, left, right)
			}
		}
		gateColumn := -1
		for index, gate := range []string{"╔═══╗", "║ ╬ ║", "╣ ║ ╠", "╚═╩═╝"} {
			line := ansiEscapePattern.ReplaceAllString(rows[index+3], "")
			before, _, found := strings.Cut(line, gate)
			if !found {
				t.Fatalf("width %d: missing gate on row %d: %q", width, index+3, line)
			}
			column := lipgloss.Width(before)
			if index == 0 {
				gateColumn = column
			} else if column != gateColumn {
				t.Fatalf("width %d: gate row %d starts at %d, want %d", width, index+3, column, gateColumn)
			}
		}
	}
}

func TestChromePacksShortcutsOnNarrowDashboard(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 90, Height: 24})
	model = updated.(appModel)
	chrome := model.chrome()
	rows := chrome.headerRows(90, 24)
	if len(rows) > 9 {
		t.Fatalf("shortcut header uses %d rows, want at most 9", len(rows))
	}
	plain := ansiEscapePattern.ReplaceAllString(strings.Join(rows, "\n"), "")
	for _, hint := range chrome.Hints {
		if !strings.Contains(plain, "<"+hint.Key+">") {
			t.Fatalf("shortcut %q missing from header:\n%s", hint.Key, plain)
		}
	}
}

func TestChromePacksShortcutsOnCompactDashboard(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	model = updated.(appModel)
	chrome := model.chrome()
	rows := chrome.headerRows(80, 30)
	if len(rows) > 9 {
		t.Fatalf("80-column header uses %d rows, want at most 9", len(rows))
	}
	plain := ansiEscapePattern.ReplaceAllString(strings.Join(rows, "\n"), "")
	for _, hint := range chrome.Hints {
		if !strings.Contains(plain, "<"+hint.Key+">") {
			t.Fatalf("shortcut %q missing from compact header:\n%s", hint.Key, plain)
		}
	}
}

func TestChromePromptFitsTerminal(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		for _, chrome := range []Chrome{
			{SearchOpen: true, Search: strings.Repeat("界", width)},
			{ShellOpen: true, Shell: strings.Repeat("界", width)},
		} {
			view := chrome.View("panel", width, 12)
			for _, row := range strings.Split(view, "\n") {
				if cells := lipgloss.Width(row); cells > width {
					t.Fatalf("width %d: row occupies %d cells: %q", width, cells, row)
				}
			}
		}
	}
}

func TestShellErrorsShowWhileShellOpen(t *testing.T) {
	// A parse error must not hide the input bar while the error is overlaid.
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
	if !strings.Contains(view, "frobnicate") || !strings.Contains(view, "|") {
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

func TestActionNotificationKeepsDashboardAndTablePosition(t *testing.T) {
	model := dashboardTestModel()
	model.options.Actions = map[string]ActionHandler{
		ActStatus: func(string) (ActionResult, error) {
			return ActionResult{}, errors.New("status unavailable")
		},
	}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(appModel)
	position := func() int {
		t.Helper()
		view := ansiEscapePattern.ReplaceAllString(model.View(), "")
		if !strings.Contains(view, "Profile") || !strings.Contains(view, "Selected") {
			t.Fatalf("dashboard disappeared:\n%s", view)
		}
		for index, line := range strings.Split(view, "\n") {
			if strings.Contains(line, "connections(all)") {
				return index
			}
		}
		t.Fatalf("connections title missing:\n%s", view)
		return -1
	}
	before := position()
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}, "i")
	model = updated.(appModel)
	if got := position(); got != before {
		t.Fatalf("table moved during action: got %d, want %d", got, before)
	}
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if got := position(); got != before || !strings.Contains(model.View(), "status unavailable") {
		t.Fatalf("error notification moved table or disappeared: row %d, want %d", got, before)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if got := position(); got != before {
		t.Fatalf("table moved after dismissal: got %d, want %d", got, before)
	}
}

func TestActionNotificationFitsMainPanel(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {80, 16}, {90, 18}, {120, 24}, {160, 30}} {
		model := dashboardTestModel()
		model.options.Actions = map[string]ActionHandler{
			ActStatus: func(string) (ActionResult, error) { return ActionResult{}, errors.New("status unavailable") },
		}
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = updated.(appModel)
		updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}, "i")
		model = updated.(appModel)
		updated, _ = model.Update(command())
		model = updated.(appModel)
		view := model.View()
		if !strings.Contains(view, "status unavailable") {
			t.Fatalf("size %v: notification missing: %q", size, view)
		}
		rows := strings.Split(view, "\n")
		if len(rows) > size[1] {
			t.Fatalf("size %v: notification produced %d rows", size, len(rows))
		}
		for _, row := range rows {
			if cells := lipgloss.Width(row); cells > size[0] {
				t.Fatalf("size %v: notification row is %d cells: %q", size, cells, row)
			}
		}
	}
}

func TestTransientNoticeKeepsPanelSize(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	model = updated.(appModel)
	before := model.View()
	beforeBudget := model.chrome().Budget(model.height, model.width)
	model.setError("status unavailable")
	after := model.View()
	if model.chrome().Budget(model.height, model.width) != beforeBudget {
		t.Fatal("notice changed panel budget")
	}
	if strings.Count(before, "\n") != strings.Count(after, "\n") || !strings.Contains(after, "status unavailable") {
		t.Fatalf("notice changed frame height or disappeared:\n%s", after)
	}
	beforeRows, afterRows := strings.Split(before, "\n"), strings.Split(after, "\n")
	for index, line := range beforeRows {
		if strings.Contains(line, "connections(all)") && line != afterRows[index] {
			t.Fatalf("notice moved or changed table title: %q", afterRows[index])
		}
	}
}

func TestDashboardTracksSelectedAccountDuringNotifications(t *testing.T) {
	summaries := []ConnSummary{
		{Name: "shared", Account: "123456789012"},
		{Name: "shared", Account: "014498740021"},
		{Name: "other", Account: "999999999999"},
	}
	model := newTestApp(summaries, RunOptions{
		Account: "123456789012 (prod) · 014498740021 (staging)",
		AccountLabels: map[string]string{
			"123456789012": "123456789012 (prod)",
			"014498740021": "014498740021 (staging)",
		},
		Actions: map[string]ActionHandler{
			ActStatus: func(string) (ActionResult, error) { return ActionResult{}, errors.New("status unavailable") },
		},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(appModel)
	account := func() string {
		for _, row := range model.dashboardMetaRows() {
			if row.Label == "Account" {
				return row.Value
			}
		}
		return ""
	}
	if got := account(); got != "123456789012 (prod)" {
		t.Fatalf("first account = %q", got)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(appModel)
	if got := account(); got != "014498740021 (staging)" {
		t.Fatalf("selected account = %q", got)
	}
	before := strings.Join(model.chrome().Dashboard, "\n")
	headerBefore := strings.Join(model.chrome().headerRows(model.width, model.height), "\n")
	updated, command := model.updateHomeLevel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")}, "i")
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if got := strings.Join(model.chrome().Dashboard, "\n"); got != before {
		t.Fatal("action notification changed selected connection metadata")
	}
	model.setError("notice")
	model.sudoPrompt = &sudoPasswordPrompt{}
	if got := strings.Join(model.chrome().Dashboard, "\n"); got != before {
		t.Fatal("notification or authorization prompt changed dashboard metadata")
	}
	if got := strings.Join(model.chrome().headerRows(model.width, model.height), "\n"); got != headerBefore {
		t.Fatal("authorization prompt changed the header layout")
	}
	model.sudoPrompt, model.actionModal = nil, nil
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(appModel)
	if got := account(); got != "999999999999" {
		t.Fatalf("unknown account alias: got %q, want account ID", got)
	}
}

func TestConfigRefreshKeepsSelectedAccountDuringNotification(t *testing.T) {
	first := &config.Config{Name: "shared", AccountID: "123456789012", SSHPort: 22}
	second := &config.Config{Name: "shared", AccountID: "014498740021", SSHPort: 22}
	model := newTestApp([]ConnSummary{{Name: first.Name, Account: first.AccountID}, {Name: second.Name, Account: second.AccountID}}, RunOptions{})
	model.knownConfigs = []*config.Config{first, second}
	model.home.SetCursor(1)
	model.actionModal = &actionModal{title: "Action failed", body: "status unavailable"}
	model.applyRefreshedConfigs([]*config.Config{second, first})
	selected, ok := model.dashboardSelection()
	if !ok || selected.Account != second.AccountID || model.home.Cursor() != 0 {
		t.Fatalf("refresh changed selected account: selected=%+v cursor=%d", selected, model.home.Cursor())
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

func TestStartAsksForSudoWhenTimestampMissing(t *testing.T) {
	var started bool
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		SudoCheck: func() error { return errors.New("sudo: a password is required") },
		Actions: map[string]ActionHandler{
			ActStart: func(string) (ActionResult, error) { started = true; return ActionResult{Title: "Tunnel started"}, nil },
		},
	})
	// The startup check has already reported the missing timestamp.
	updated, _ := model.Update(sudoCheckedMsg{err: errors.New("sudo: a password is required")})
	model = updated.(appModel)
	updated, command := model.requestAction(ActStart, "corp", ViewHome)
	model = updated.(appModel)
	if command != nil || started || model.outcome.Action != "" {
		t.Fatalf("start must be parked behind the sudo prompt: cmd=%v started=%v outcome=%+v", command != nil, started, model.outcome)
	}
	if model.actionModal == nil || model.actionModal.confirmAction != sudoAuthConfirm || model.sudoPending == nil {
		t.Fatalf("sudo prompt modal missing: modal=%+v pending=%+v", model.actionModal, model.sudoPending)
	}
	if model.sudo != sudoMissing {
		t.Fatalf("sudo state = %v, want sudoMissing", model.sudo)
	}
	// Esc cancels the parked request entirely.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.actionModal != nil || model.sudoPending != nil || started {
		t.Fatal("skipping sudo should clear the prompt and the pending action")
	}
}

func TestSudoAuthenticationResumesParkedStart(t *testing.T) {
	cached := false
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		SudoCheck: func() error {
			if cached {
				return nil
			}
			return errors.New("sudo: a password is required")
		},
		SudoAuthenticate: func(password string) error {
			cached = password == "secret"
			return nil
		},
		Actions: map[string]ActionHandler{
			ActStart: func(string) (ActionResult, error) { return ActionResult{Title: "Tunnel started"}, nil },
		},
	})
	updated, _ := model.requestAction(ActStart, "corp", ViewHome)
	model = updated.(appModel)
	// Enter opens a masked password prompt inside the TUI.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if model.sudoPrompt == nil || !model.sudoPrompt.authenticate || model.actionModal != nil || model.sudoPending == nil {
		t.Fatalf("enter must open password prompt and keep pending action: prompt=%v modal=%v pending=%v", model.sudoPrompt != nil, model.actionModal, model.sudoPending)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("secret")})
	model = updated.(appModel)
	var authCommand tea.Cmd
	updated, authCommand = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	if authCommand == nil || model.sudoPrompt != nil {
		t.Fatal("password submit did not start background sudo validation")
	}
	updated, verifyCommand := model.Update(authCommand())
	model = updated.(appModel)
	if model.sudo != sudoUnknown || verifyCommand == nil {
		t.Fatalf("after auth the timestamp must be re-verified: state=%v cmd=%v", model.sudo, verifyCommand != nil)
	}
	updated, startCommand := model.Update(verifyCommand())
	model = updated.(appModel)
	if model.sudo != sudoReady {
		t.Fatalf("verified timestamp must mark ready before resuming: state=%v", model.sudo)
	}
	if model.actionModal == nil || !model.actionModal.running || model.sudoPending != nil {
		t.Fatalf("resumed start should be running without a pending request: modal=%+v pending=%v", model.actionModal, model.sudoPending)
	}
	if startCommand == nil {
		t.Fatal("parked start and keep-warm commands were not scheduled")
	}
	// The keep-warm ticker must be armed.
	updated, warmCommand := model.Update(sudoWarmTickMsg{})
	model = updated.(appModel)
	if warmCommand == nil {
		t.Fatal("keep-warm ticker not armed after authentication")
	}
	updated, _ = model.Update(warmCommand())
	model = updated.(appModel)
	if model.sudo != sudoReady {
		t.Fatalf("successful warm refresh must keep state ready: %v", model.sudo)
	}
}

func TestSudoWarmExpiryFlagsMissingAgain(t *testing.T) {
	valid := true
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{
		SudoCheck: func() error {
			if valid {
				return nil
			}
			return errors.New("sudo: a password is required")
		},
		Actions: map[string]ActionHandler{},
	})
	updated, _ := model.Update(sudoCheckedMsg{err: nil})
	model = updated.(appModel)
	if model.sudo != sudoReady {
		t.Fatalf("initial check: state = %v, want sudoReady", model.sudo)
	}
	valid = false
	updated, _ = model.Update(sudoWarmTickMsg{})
	model = updated.(appModel)
	updated, _ = model.Update(sudoWarmedMsg{err: errors.New("sudo: a password is required")})
	model = updated.(appModel)
	if model.sudo != sudoMissing {
		t.Fatalf("expired timestamp must flip to missing: %v", model.sudo)
	}
	view := model.View()
	if !strings.Contains(view, "sudo needed") {
		t.Fatal("status bar must show the sudo warning when the timestamp is missing")
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
	for _, expected := range []string{"NORMAL", "Field 5/19 · target", "▶ target"} {
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

func TestEditorSelectsAWSProfiles(t *testing.T) {
	cfg := &config.Config{Name: "corp", AWSProfile: "default"}
	model := newTestApp(nil, RunOptions{AWSProfiles: []string{"default", "production"}})
	model.editor = newConnectionEditor(cfg, model.options.AWSProfiles)
	model.editor.field = len(model.editor.fields) - 1
	model.editor.input = model.editor.values[model.editor.field]
	updated, _ := model.updateEditor("enter", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("v", []rune("v"))
	model = updated.(appModel)
	updated, _ = model.updateEditor("down", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if model.editor.values[model.editor.field] != "production" {
		t.Fatalf("selected profile = %q, want production", model.editor.values[model.editor.field])
	}
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("up", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if model.editor.values[model.editor.field] != "default" {
		t.Fatalf("selected profile = %q, want default", model.editor.values[model.editor.field])
	}
}

func TestVimSaveRefreshBeforeCompletion(t *testing.T) {
	cfg := &config.Config{Name: "corp", AccountID: "123456789012", Kind: config.ProfileKindVPC, SSHPort: 22}
	model := newTestApp([]ConnSummary{{Name: cfg.Name, Account: cfg.AccountID}}, RunOptions{SaveConfig: func(string, *config.Config) error { return nil }})
	model.knownConfigs = []*config.Config{cfg}
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.values[4] = "new-target"
	updated, command := model.saveVimEditor(false)
	model = updated.(appModel)
	message := command().(vimSaveFinishedMsg)
	model.applyRefreshedConfigs([]*config.Config{message.cfg})
	updated, _ = model.Update(message)
	model = updated.(appModel)
	if model.editor.conflicted || model.editor.isDirty() {
		t.Fatalf("own save was treated as a conflicting external edit: %+v", model.editor)
	}
}

func TestEditorEscapeConfirmsUnsavedChanges(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.values[0] = "changed"
	model.editor.vimInsert = true
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.editor == nil || !model.editor.discardConfirm || !strings.Contains(model.View(), "Discard unsaved changes?") {
		t.Fatal("escape did not confirm before discarding")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model = updated.(appModel)
	if model.editor == nil || model.editor.discardConfirm || model.editor.values[0] != "changed" {
		t.Fatal("canceling confirmation did not preserve the draft")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = updated.(appModel)
	if model.editor != nil {
		t.Fatal("confirming discard did not return to the parent")
	}
}

func TestEditorEscapeReturnsToParent(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.editor == nil || model.editor.yamlMode {
		t.Fatal("clean YAML editor did not return to the form")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.editor != nil {
		t.Fatal("clean form did not return to the parent")
	}
}

func TestEditorEscapeConfirmsNewAndYAMLDrafts(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.isNew = true
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if !model.editor.discardConfirm {
		t.Fatal("new connection did not request discard confirmation")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	model = updated.(appModel)
	model.editor.yamlLines[0] = "name: changed"
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if !model.editor.discardConfirm {
		t.Fatal("modified YAML did not request discard confirmation")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = updated.(appModel)
	if model.editor == nil || model.editor.yamlMode || model.editor.values[0] != "corp" {
		t.Fatal("discarding YAML did not restore the parent form")
	}
}

func TestEditorEscapeConfirmsRouteChanges(t *testing.T) {
	cfg := &config.Config{Name: "corp", SSHPort: 22, Routes: []config.NetworkRoute{{CIDR: "10.0.0.0/16", Selected: true}}}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.field = 12
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeySpace})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if !model.editor.discardConfirm || model.editor.routes[0].Selected {
		t.Fatal("route changes did not request confirmation")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model = updated.(appModel)
	if !model.editor.routePicker || model.editor.routes[0].Selected {
		t.Fatal("canceling discard lost the route draft")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = updated.(appModel)
	if model.editor.routePicker || !model.editor.routes[0].Selected {
		t.Fatal("confirming discard did not restore the parent form")
	}
}

func TestEditorKeepsHeaderWithoutConnectionsTable(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	for _, size := range [][2]int{{80, 24}, {160, 30}} {
		model := newTestApp([]ConnSummary{{Name: "corp"}, {Name: "other"}}, RunOptions{})
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = updated.(appModel)
		before := strings.Join(model.chrome().Dashboard, "\n")
		headerHeight := len(model.chrome().headerRows(model.width, model.height))
		model.editor = newConnectionEditor(cfg, nil)
		view := ansiEscapePattern.ReplaceAllString(model.View(), "")
		if strings.Contains(view, "connections(all)") || !strings.Contains(view, "edit(corp)") {
			t.Fatalf("size %v: editor view contains connection table or lacks editor:\n%s", size, view)
		}
		if got := strings.Join(model.chrome().Dashboard, "\n"); got != before {
			t.Fatalf("size %v: editor changed dashboard metadata", size)
		}
		if got := len(model.chrome().headerRows(model.width, model.height)); got != headerHeight {
			t.Fatalf("size %v: editor changed header height from %d to %d", size, headerHeight, got)
		}
		if lines := strings.Count(view, "\n") + 1; lines > size[1] {
			t.Fatalf("size %v: view has %d lines", size, lines)
		}
	}
}

func TestEditorViewsFitTerminal(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	for _, size := range [][2]int{{40, 12}, {60, 16}, {80, 18}, {90, 18}, {120, 20}, {160, 30}} {
		model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = updated.(appModel)
		model.editor = newConnectionEditor(cfg, nil)
		for _, yamlMode := range []bool{false, true} {
			if yamlMode {
				if err := enterYAMLEditor(model.editor); err != nil {
					t.Fatal(err)
				}
			}
			view := model.View()
			if lines := strings.Count(view, "\n") + 1; lines > size[1] {
				t.Fatalf("size %v YAML %v: view has %d lines", size, yamlMode, lines)
			}
			for _, line := range strings.Split(view, "\n") {
				if cells := lipgloss.Width(line); cells > size[0] {
					t.Fatalf("size %v YAML %v: %d-cell line: %q", size, yamlMode, cells, line)
				}
			}
		}
	}
}

func TestVimKeyboardWorkflow(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	var saved *config.Config
	model := newTestApp(nil, RunOptions{SaveConfig: func(_ string, draft *config.Config) error { saved = draft; return nil }})
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.input = model.editor.values[0]
	press := func(key tea.KeyMsg) tea.Cmd {
		t.Helper()
		updated, command := model.Update(key)
		model = updated.(appModel)
		return command
	}
	press(tea.KeyMsg{Type: tea.KeyCtrlY})
	model.editor.yamlCol = 6
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new-")})
	press(tea.KeyMsg{Type: tea.KeyCtrlO})
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	if !strings.Contains(view, ":|") || !strings.Contains(view, "yaml ·") {
		t.Fatalf("editor command prompt missing: %q", view)
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("wq")})
	command := press(tea.KeyMsg{Type: tea.KeyEnter})
	if command == nil {
		t.Fatal(":wq did not start a save")
	}
	updated, _ := model.Update(command())
	model = updated.(appModel)
	if saved == nil || saved.Name != "new-corp" || model.editor == nil || model.editor.yamlMode {
		t.Fatalf("YAML :wq result: saved=%+v editor=%+v", saved, model.editor)
	}
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	press(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	press(tea.KeyMsg{Type: tea.KeyEnter})
	if model.editor != nil {
		t.Fatal(":q did not close the unchanged form")
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
	updated, _ := model.updateEditor("enter", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("down", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
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

func TestEscAtHomeDoesNotQuit(t *testing.T) {
	model := newTestApp([]ConnSummary{{Name: "corp"}}, RunOptions{})
	model.outcome = Outcome{View: ViewHome}
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if command != nil || model.outcome.Quit {
		t.Fatalf("esc at home quit the app: outcome = %+v, cmd = %v", model.outcome, command != nil)
	}
	if model.level() != 0 || model.outcome.View != ViewHome {
		t.Fatalf("esc at home changed the view: %+v", model.outcome)
	}
}

func TestNoticeCountsDownAndClears(t *testing.T) {
	model := newTestApp(nil, RunOptions{})
	model.setError("boom")
	if got := model.notice(); got != "5s · boom" {
		t.Fatalf("notice = %q, want %q", got, "5s · boom")
	}
	for remaining := 4; remaining >= 1; remaining-- {
		updated, command := model.Update(noticeTickMsg{})
		model = updated.(appModel)
		if command == nil {
			t.Fatalf("notice tick did not reschedule itself")
		}
		want := fmt.Sprintf("%ds · boom", remaining)
		if got := model.notice(); got != want {
			t.Fatalf("notice = %q, want %q", got, want)
		}
	}
	updated, _ := model.Update(noticeTickMsg{})
	model = updated.(appModel)
	if model.errorMsg != "" || model.notice() != "" {
		t.Fatalf("notice did not clear: %q", model.notice())
	}
}

func TestNewNoticeRestartsCountdown(t *testing.T) {
	model := newTestApp(nil, RunOptions{})
	model.setError("first")
	updated, _ := model.Update(noticeTickMsg{})
	model = updated.(appModel)
	if got := model.notice(); got != "4s · first" {
		t.Fatalf("notice = %q, want %q", got, "4s · first")
	}
	model.setError("second")
	if got := model.notice(); got != "5s · second" {
		t.Fatalf("new notice did not restart the countdown: %q", got)
	}
}

func TestNoticeAppearsInViewWithCountdown(t *testing.T) {
	model := newTestApp(nil, RunOptions{})
	model.setError("boom")
	if view := model.View(); !strings.Contains(view, "5s · boom") {
		t.Fatalf("view missing notice countdown:\n%s", view)
	}
}

func TestEditorMovesCaretAndEditsInPlace(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.field = 4 // target (free text)
	model.editor.values[4] = "abcdef"
	model.editor.input = "abcdef"
	model.editor.cursor = editorCursorEnd(model.editor.input)

	apply := func(key string, runes []rune) {
		t.Helper()
		updated, _ := model.updateEditor(key, runes)
		model = updated.(appModel)
	}

	apply("left", nil)
	apply("left", nil)
	if model.editor.cursor != 4 {
		t.Fatalf("caret = %d, want 4", model.editor.cursor)
	}
	apply("i", []rune("i"))
	apply("", []rune("X"))
	if got := model.editor.values[4]; got != "abcdXef" {
		t.Fatalf("typed in place = %q, want %q", got, "abcdXef")
	}
	if model.editor.cursor != 5 {
		t.Fatalf("caret after typing = %d, want 5", model.editor.cursor)
	}

	apply("backspace", nil)
	if got := model.editor.values[4]; got != "abcdef" {
		t.Fatalf("backspace = %q, want %q", got, "abcdef")
	}

	apply("home", nil)
	apply("delete", nil)
	if got := model.editor.values[4]; got != "bcdef" {
		t.Fatalf("delete at home = %q, want %q", got, "bcdef")
	}

	apply("end", nil)
	apply("left", nil)
	apply("", []rune("Z"))
	if got := model.editor.values[4]; got != "bcdeZf" {
		t.Fatalf("insert before last rune = %q, want %q", got, "bcdeZf")
	}
}

func TestEditorCaretIsRuneBased(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.field = 4
	model.editor.values[4] = "héllo"
	model.editor.input = "héllo"
	model.editor.cursor = editorCursorEnd(model.editor.input)

	updated, _ := model.updateEditor("left", nil)
	model = updated.(appModel)
	if model.editor.cursor != 4 {
		t.Fatalf("caret = %d, want 4 runes", model.editor.cursor)
	}
	updated, _ = model.updateEditor("i", []rune("i"))
	model = updated.(appModel)
	updated, _ = model.updateEditor("", []rune("!"))
	model = updated.(appModel)
	if got := model.editor.values[4]; got != "héll!o" {
		t.Fatalf("typed in place = %q, want %q", got, "héll!o")
	}
}

func TestEditorRendersCaretAtCursor(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	editor := newConnectionEditor(cfg, nil)
	editor.field = 4
	editor.values[4] = "abcdef"
	editor.input = "abcdef"
	editor.cursor = 2
	view := renderEditor(editor, 8, "")
	if !strings.Contains(ansiEscapePattern.ReplaceAllString(view, ""), "abcdef") || strings.Contains(view, "ab|cdef") {
		t.Fatalf("caret was not drawn at the cursor:\n%s", view)
	}
}

func TestEditorSplitShowsYAMLPreview(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", Region: "eu-central-1", Target: "i-1", SSHPort: 22}
	editor := newConnectionEditor(cfg, nil)
	view := ansiEscapePattern.ReplaceAllString(renderEditorSplit(editor, 40, 140, ""), "")
	for _, expected := range []string{"YAML PREVIEW", "name: corp", "connection_method: ssm", "│"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("split editor missing %q:\n%s", expected, view)
		}
	}
}

func TestEditorSplitShowsValidationError(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	editor := newConnectionEditor(cfg, nil)
	editor.values[7] = "not-a-port"
	view := renderEditorSplit(editor, 40, 140, "")
	if !strings.Contains(view, "invalid SSH port") {
		t.Fatalf("split editor did not surface the draft error:\n%s", view)
	}
}

func TestYAMLEditorAppliesEdits(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", Region: "eu-central-1", Target: "i-1", SSHPort: 22}
	model := newTestApp(nil, RunOptions{SaveConfig: func(string, *config.Config) error { return nil }})
	model.editor = newConnectionEditor(cfg, nil)

	updated, _ := model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	if !model.editor.yamlMode {
		t.Fatal("ctrl+y did not open the YAML editor")
	}
	replaced := false
	for index, line := range model.editor.yamlLines {
		if line == "name: corp" {
			model.editor.yamlLines[index] = "name: renamed"
			replaced = true
			break
		}
	}
	if !replaced {
		t.Fatalf("YAML draft has no name line: %v", model.editor.yamlLines)
	}

	updated, command := model.updateEditor("ctrl+s", nil)
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if !model.editor.yamlMode {
		t.Fatal("ctrl+s closed the YAML editor")
	}
	if got := model.editor.values[0]; got != "renamed" {
		t.Fatalf("name = %q, want renamed", got)
	}
}

func TestYAMLEditorCancelDiscardsEdits(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	updated, _ := model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	model.editor.yamlLines[0] = "name: discarded"

	model.editor.vimPrompt, model.editor.vimInput = ":", "q!"
	updated, _ = model.updateVimPrompt("enter", nil)
	model = updated.(appModel)
	if model.editor.yamlMode {
		t.Fatal("esc did not leave the YAML editor")
	}
	if got := model.editor.values[0]; got != "corp" {
		t.Fatalf("esc applied YAML edits: name = %q", got)
	}
}

func TestYAMLEditorMultiLineEditing(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	updated, _ := model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	original := len(model.editor.yamlLines)

	updated, _ = model.updateEditor("i", []rune("i"))
	model = updated.(appModel)
	updated, _ = model.updateEditor("", []rune("# "))
	model = updated.(appModel)
	if got := model.editor.yamlLines[0]; got != "# name: corp" {
		t.Fatalf("typing at start = %q", got)
	}
	updated, _ = model.updateEditor("end", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	if len(model.editor.yamlLines) != original+1 || model.editor.yamlRow != 1 {
		t.Fatalf("enter did not split the line: rows=%d row=%d", len(model.editor.yamlLines), model.editor.yamlRow)
	}
	updated, _ = model.updateEditor("backspace", nil)
	model = updated.(appModel)
	if len(model.editor.yamlLines) != original || model.editor.yamlRow != 0 {
		t.Fatalf("backspace did not merge the line: rows=%d row=%d", len(model.editor.yamlLines), model.editor.yamlRow)
	}
}

func TestVimEditorModesAndUndo(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	model.editor.input = model.editor.values[0]
	updated, _ := model.updateEditor("i", []rune("i"))
	model = updated.(appModel)
	updated, _ = model.updateEditor("x", []rune("x"))
	model = updated.(appModel)
	if model.editor.values[0] != "xcorp" {
		t.Fatalf("insert mode: %q", model.editor.values[0])
	}
	updated, _ = model.updateEditor("esc", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("u", []rune("u"))
	model = updated.(appModel)
	if model.editor.values[0] != "corp" || model.editor.vimInsert {
		t.Fatalf("undo/mode: %+v", model.editor)
	}
	updated, _ = model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	updated, _ = model.updateEditor("o", []rune("o"))
	model = updated.(appModel)
	if !model.editor.vimInsert || model.editor.yamlRow != 1 {
		t.Fatalf("open line: %+v", model.editor)
	}
}

func TestVimSavePreservesDraftOnFailure(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	model := newTestApp(nil, RunOptions{SaveConfig: func(string, *config.Config) error { return errors.New("disk full") }})
	model.editor = newConnectionEditor(cfg, nil)
	updated, _ := model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	model.editor.yamlLines[0] = "name: renamed"
	updated, command := model.saveVimEditor(false)
	model = updated.(appModel)
	if command == nil || !model.editor.vimSaving {
		t.Fatal("save did not start")
	}
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if model.editor == nil || !model.editor.yamlMode || model.editor.yamlLines[0] != "name: renamed" || !strings.Contains(model.errorMsg, "disk full") {
		t.Fatalf("failed save lost draft: editor=%+v error=%q", model.editor, model.errorMsg)
	}
	model.editor.vimPrompt, model.editor.vimInput = ":", "q"
	updated, _ = model.updateVimPrompt("enter", nil)
	model = updated.(appModel)
	if !model.editor.yamlMode || !strings.Contains(model.errorMsg, "unsaved") {
		t.Fatal(":q did not protect unsaved changes")
	}
	model.editor.vimPrompt, model.editor.vimInput = ":", "q!"
	updated, _ = model.updateVimPrompt("enter", nil)
	model = updated.(appModel)
	if model.editor.yamlMode || model.editor.values[0] != "corp" {
		t.Fatal(":q! did not discard YAML draft")
	}
}

func TestVimSaveKeepsYAMLOpen(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	var saved *config.Config
	model := newTestApp(nil, RunOptions{SaveConfig: func(_ string, draft *config.Config) error { saved = draft; return nil }})
	model.editor = newConnectionEditor(cfg, nil)
	updated, _ := model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	model.editor.yamlLines[0] = "name: renamed"
	updated, command := model.saveVimEditor(false)
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	if saved == nil || saved.Name != "renamed" || !model.editor.yamlMode || model.editor.isDirty() || model.editor.originalName != "renamed" {
		t.Fatalf("save result: saved=%+v editor=%+v", saved, model.editor)
	}
}

func TestVimNormalModeDoesNotInsertLines(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	updated, _ := model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	before := strings.Join(model.editor.yamlLines, "\n")
	updated, _ = model.updateEditor("enter", nil)
	model = updated.(appModel)
	model.editor.yamlCol = len([]rune(model.editor.yamlLines[0]))
	updated, _ = model.updateEditor("x", []rune("x"))
	model = updated.(appModel)
	if got := strings.Join(model.editor.yamlLines, "\n"); got != before {
		t.Fatalf("normal mode unexpectedly changed YAML: %q", got)
	}
}

func TestVimLinePreservesTextAndCursorVisibility(t *testing.T) {
	for _, line := range []string{"name: \"a:#b\" # comment", "number: 42", "enabled: true", "路径: hello"} {
		got := ansiEscapePattern.ReplaceAllString(renderVimLine(line, 2, false, true, 0), "")
		if got != line {
			t.Fatalf("YAML rendering changed text: got %q, want %q", got, line)
		}
	}
	line := "prefix-界界-tail"
	rendered := renderVimLine(line, len([]rune(line))-1, false, false, 6)
	if lipgloss.Width(rendered) > 6 || !strings.Contains(ansiEscapePattern.ReplaceAllString(rendered, ""), "l") {
		t.Fatalf("cursor clipped: %q", rendered)
	}
}

func TestVimSearchUsesRuneOffsets(t *testing.T) {
	editor := &connectionEditor{yamlLines: []string{"İ value target"}}
	editor.searchYAML("target")
	if editor.yamlCol != 8 {
		t.Fatalf("search cursor = %d, want 8", editor.yamlCol)
	}
}

func TestYAMLEditorOwnsSlashKey(t *testing.T) {
	cfg := &config.Config{Name: "corp", Kind: config.ProfileKindVPC, ConnectionMethod: "ssm", SSHPort: 22}
	model := newTestApp(nil, RunOptions{})
	model.editor = newConnectionEditor(cfg, nil)
	updated, _ := model.updateEditor("ctrl+y", nil)
	model = updated.(appModel)
	model.editor.yamlRow, model.editor.yamlCol = 0, 0
	updated, _ = model.updateEditor("i", []rune("i"))
	model = updated.(appModel)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(appModel)
	if model.searching {
		t.Fatal("/ opened search instead of editing YAML")
	}
	if !strings.HasPrefix(model.editor.yamlLines[0], "/") {
		t.Fatalf("slash was not inserted: %q", model.editor.yamlLines[0])
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

func TestConnectionPanelFillsTerminalHeight(t *testing.T) {
	detail := &ConnDetail{Name: "corp", Method: "ssh", Target: "i-1", TargetOK: true}
	model := appModel{options: RunOptions{Level: 1, Detail: detail}, home: newHomeTable(nil, 0)}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	view := updated.(appModel).View()
	if lines := strings.Count(view, "\n") + 1; lines != 20 {
		t.Fatalf("connection view has %d lines, want terminal height 20:\n%s", lines, view)
	}
	rows := strings.Split(view, "\n")
	if !strings.Contains(rows[0], "BASTIONCTL") {
		t.Fatalf("top bar missing on the first row:\n%s", view)
	}
	if !strings.Contains(view, "connection(corp)") {
		t.Fatalf("panel title missing on the first body row:\n%s", view)
	}
	if !strings.Contains(rows[18], "└") {
		t.Fatalf("panel does not close above the footer:\n%s", view)
	}
	if !strings.Contains(rows[19], project.Repository) {
		t.Fatalf("project footer missing:\n%s", view)
	}
	if !strings.Contains(rows[0], "<esc>") {
		t.Fatalf("header hint bar missing:\n%s", view)
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
	updated, _ = model.updateEditor("i", []rune("i"))
	model = updated.(appModel)
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

func dashboardTestModel() appModel {
	summaries := []ConnSummary{
		{Name: "corp", Account: "698715940080", Kind: "vpc", Method: "ssm", Region: "eu-west-1", Networks: "172.16.0.0/12", DNS: "split (2 domains)", Running: true, PID: 4242},
		{Name: "ci-edge", Account: "698715940080", Kind: "dedicated", Method: "ssh", Region: "us-east-1", DNS: "off"},
	}
	return newTestApp(summaries, RunOptions{
		Account:       "698715940080 (prod)",
		AccountLabels: map[string]string{"698715940080": "698715940080 (prod)"},
		Profile:       "staging",
		Highlight:     0,
		Version:       "v1.2.3",
	})
}

func TestDashboardRendersAboveTable(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(appModel)
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	for _, expected := range []string{
		"BASTIONCTL", "698715940080 (prod)",
		"Profile", "staging",
		"Connections", "2 saved · 1 running",
		"Selected", "corp",
		"Method", "ssm · eu-west-1",
		"DNS", "split (2 domains)",
		"Bastionctl", "v1.2.3",
		"<enter>", "open",
		"<v>", "check AWS",
		"<q>", "quit",
		"connections(all)[2]",
		"┌", "└", "│",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("dashboard view missing %q:\n%s", expected, view)
		}
	}
}

func TestDashboardPersistsAcrossKeys(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(appModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(appModel)
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	if !strings.Contains(view, "Profile") || !strings.Contains(view, "connections(all)[2]") {
		t.Fatalf("dashboard disappeared after a key:\n%s", view)
	}
}

func TestDashboardHiddenOnNarrowTerminal(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 50, Height: 24})
	model = updated.(appModel)
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	if strings.Contains(view, "Selected") {
		t.Fatalf("narrow terminal still rendered the dashboard metadata:\n%s", view)
	}
	if !strings.Contains(view, "BASTIONCTL") {
		t.Fatalf("narrow terminal lost the top bar:\n%s", view)
	}
}

func TestDashboardStaysInConnectionView(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	model = updated.(appModel)
	before := strings.Join(model.chrome().Dashboard, "\n")
	headerHeight := len(model.chrome().headerRows(model.width, model.height))
	model.options.Level = 1
	model.options.Detail = &ConnDetail{Name: "corp", Kind: "vpc", Method: "ssm", Region: "eu-west-1", Target: "i-1", TargetOK: true}
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	if got := strings.Join(model.chrome().Dashboard, "\n"); got != before {
		t.Fatalf("connection view changed dashboard metadata: %q, want %q", got, before)
	}
	if got := len(model.chrome().headerRows(model.width, model.height)); got != headerHeight {
		t.Fatalf("header height changed from %d to %d", headerHeight, got)
	}
	if strings.Contains(view, "connections(all)") || !strings.Contains(view, "connection(corp)") {
		t.Fatalf("connection view contains list or lacks details:\n%s", view)
	}
	if !strings.Contains(view, "BASTIONCTL") {
		t.Fatalf("connection view lost the top bar:\n%s", view)
	}
}

func TestDashboardStaysAcrossViews(t *testing.T) {
	for _, size := range [][2]int{{60, 16}, {80, 24}, {160, 30}} {
		model := dashboardTestModel()
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = updated.(appModel)
		before := strings.Join(model.chrome().Dashboard, "\n")
		height := len(model.chrome().headerRows(model.width, model.height))
		if before == "" {
			t.Fatalf("size %v: initial dashboard missing", size)
		}
		views := []struct {
			name  string
			apply func(*appModel)
		}{
			{"connection", func(m *appModel) { m.options.Level = 1; m.options.Detail = &ConnDetail{Name: "corp"} }},
			{"editor", func(m *appModel) { m.editor = newConnectionEditor(&config.Config{Name: "corp", SSHPort: 22}, nil) }},
			{"rescan", func(m *appModel) { m.rescan = &rescanSelector{connection: "corp"} }},
			{"wizard", func(m *appModel) { m.newWizard = &newConnectionWizard{step: "kind"} }},
			{"logs", func(m *appModel) { m.logSession = &LogSession{} }},
			{"help", func(m *appModel) { m.help = true }},
		}
		for _, view := range views {
			current := model
			view.apply(&current)
			if got := strings.Join(current.chrome().Dashboard, "\n"); got != before {
				t.Fatalf("size %v %s: dashboard changed: %q, want %q", size, view.name, got, before)
			}
			if got := len(current.chrome().headerRows(current.width, current.height)); got != height {
				t.Fatalf("size %v %s: header height changed from %d to %d", size, view.name, height, got)
			}
		}
	}
}

func TestConnectionTransitionKeepsHeaderStable(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {60, 16}, {80, 18}, {90, 24}, {100, 24}, {120, 30}, {160, 30}} {
		model := dashboardTestModel()
		model.options.LoadDetail = func(name string) (*ConnDetail, error) {
			return &ConnDetail{Name: name, Method: "ssm", Region: "eu-west-1", TargetOK: true}, nil
		}
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = updated.(appModel)
		before := strings.Join(model.chrome().Dashboard, "\n")
		height := len(model.chrome().headerRows(model.width, model.height))
		updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		model = updated.(appModel)
		if got := len(model.chrome().headerRows(model.width, model.height)); got != height {
			t.Fatalf("size %v: loading changed header height from %d to %d", size, height, got)
		}
		if !strings.Contains(model.View(), "loading corp") {
			t.Fatalf("size %v: loading indicator missing", size)
		}
		if command == nil {
			t.Fatal("opening connection did not load details")
		}
		updated, _ = model.Update(command())
		model = updated.(appModel)
		if got := strings.Join(model.chrome().Dashboard, "\n"); got != before {
			t.Fatalf("size %v: connection changed dashboard: %q, want %q", size, got, before)
		}
		if got := len(model.chrome().headerRows(model.width, model.height)); got != height {
			t.Fatalf("size %v: connection changed header height from %d to %d", size, height, got)
		}
		view := ansiEscapePattern.ReplaceAllString(model.View(), "")
		if strings.Contains(view, "connections(all)") || !strings.Contains(view, "connection(corp)") {
			t.Fatalf("size %v: detail view contains list or lacks details:\n%s", size, view)
		}
		if lines := strings.Count(view, "\n") + 1; lines > size[1] {
			t.Fatalf("size %v: connection view has %d lines", size, lines)
		}
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
		model = updated.(appModel)
		if model.level() != 0 || len(model.chrome().headerRows(model.width, model.height)) != height {
			t.Fatalf("size %v: returning home changed header or level", size)
		}
	}
}

func TestShowEditAndBackKeepHeader(t *testing.T) {
	model := dashboardTestModel()
	model.options.LoadDetail = func(name string) (*ConnDetail, error) {
		return &ConnDetail{Name: name, TargetOK: true}, nil
	}
	model.options.LoadConfig = func(name string) (*config.Config, error) {
		return &config.Config{Name: name, SSHPort: 22}, nil
	}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)
	before := strings.Join(model.chrome().Dashboard, "\n")
	height := len(model.chrome().headerRows(model.width, model.height))
	check := func(title string) {
		t.Helper()
		if got := strings.Join(model.chrome().Dashboard, "\n"); got != before {
			t.Fatalf("%s changed dashboard", title)
		}
		if got := len(model.chrome().headerRows(model.width, model.height)); got != height {
			t.Fatalf("%s changed header height from %d to %d", title, height, got)
		}
		view := model.View()
		if !strings.Contains(view, title) || strings.Contains(view, "connections(all)") {
			t.Fatalf("%s did not use a single panel:\n%s", title, view)
		}
	}
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(appModel)
	updated, _ = model.Update(command())
	model = updated.(appModel)
	check("connection(corp)")
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	model = updated.(appModel)
	if command == nil {
		t.Fatal("edit did not load configuration")
	}
	updated, _ = model.Update(command())
	model = updated.(appModel)
	check("edit(corp)")
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	check("connection(corp)")
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(appModel)
	if model.level() != 0 || strings.Join(model.chrome().Dashboard, "\n") != before || len(model.chrome().headerRows(model.width, model.height)) != height {
		t.Fatal("returning home changed the dashboard")
	}
}

func TestLogViewKeepsHeaderAndShowsNotifications(t *testing.T) {
	model := dashboardTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updated.(appModel)
	before := strings.Join(model.chrome().Dashboard, "\n")
	model.logSession = &LogSession{}
	model.logTitle = "Live sshuttle logs"
	model.logLines = []string{"log entry"}
	model.actionModal = &actionModal{title: "Action failed", body: "status unavailable", failed: true}
	view := model.View()
	if got := strings.Join(model.chrome().Dashboard, "\n"); got != before {
		t.Fatal("log notification changed the dashboard")
	}
	for _, text := range []string{"Live sshuttle logs", "Action failed", "status unavailable"} {
		if !strings.Contains(view, text) {
			t.Fatalf("log view missing %q:\n%s", text, view)
		}
	}
}

func TestConnectionDashboardKeepsSelectedAccount(t *testing.T) {
	summaries := []ConnSummary{
		{Name: "shared", Account: "123456789012"},
		{Name: "shared", Account: "014498740021"},
	}
	model := newTestApp(summaries, RunOptions{
		Highlight: 1,
		AccountLabels: map[string]string{
			"123456789012": "123456789012 (prod)",
			"014498740021": "014498740021 (staging)",
		},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	model = updated.(appModel)
	model.options.Level = 1
	model.options.Detail = &ConnDetail{Name: "shared"}
	view := ansiEscapePattern.ReplaceAllString(model.View(), "")
	if !strings.Contains(view, "014498740021 (staging)") || strings.Contains(view, "123456789012 (prod)") {
		t.Fatalf("connection dashboard does not show the selected account:\n%s", view)
	}
}

func TestDashboardLayoutsFitWidths(t *testing.T) {
	model := dashboardTestModel()
	model.options.AccountLabels["698715940080"] = strings.Repeat("界", 100)
	for _, width := range []int{200, 160, 130, 120, 100, 90} {
		rows := renderDashboardLines(model, width)
		plain := ansiEscapePattern.ReplaceAllString(strings.Join(rows, "\n"), "")
		if len(rows) == 0 || !strings.Contains(plain, "Profile") {
			t.Fatalf("dashboard at width %d lost the metadata", width)
		}
		for _, row := range rows {
			plainRow := ansiEscapePattern.ReplaceAllString(row, "")
			if got := lipgloss.Width(plainRow); got > width {
				t.Fatalf("dashboard row at width %d is %d cells wide: %q", width, got, plainRow)
			}
		}
	}
}

func TestAppViewsFitTerminal(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {80, 18}, {90, 18}, {90, 20}, {100, 24}, {120, 18}, {120, 20}, {160, 30}} {
		model := dashboardTestModel()
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		model = updated.(appModel)
		for _, key := range []string{"", "/", "esc", ":"} {
			if key != "" {
				message := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
				if key == "esc" {
					message = tea.KeyMsg{Type: tea.KeyEsc}
				}
				updated, _ = model.Update(message)
				model = updated.(appModel)
			}
			rows := strings.Split(model.View(), "\n")
			if len(rows) > size[1] {
				t.Fatalf("size %v key %q: %d rows", size, key, len(rows))
			}
			for _, row := range rows {
				if cells := lipgloss.Width(row); cells > size[0] {
					t.Fatalf("size %v key %q: %d-cell row: %q", size, key, cells, row)
				}
			}
		}
	}
}

func TestTablePanelTitleAndBorder(t *testing.T) {
	panel := ansiEscapePattern.ReplaceAllString(renderTablePanel("HEADER\nrow", 40, 4, "connections(all)[2]"), "")
	top := strings.Split(panel, "\n")[0]
	before, after, ok := strings.Cut(top, "connections(all)[2]")
	if !ok || max(lipgloss.Width(before), lipgloss.Width(after))-min(lipgloss.Width(before), lipgloss.Width(after)) > 1 {
		t.Fatalf("panel title is not centered: %q", top)
	}
	for _, expected := range []string{"connections(all)[2]", "┌", "└", "│", "HEADER", "row"} {
		if !strings.Contains(panel, expected) {
			t.Fatalf("table panel missing %q:\n%s", expected, panel)
		}
	}
	for _, line := range strings.Split(panel, "\n") {
		if got := len([]rune(line)); got != 40 {
			t.Fatalf("panel line is %d cells, want 40: %q", got, line)
		}
	}
	for _, line := range strings.Split(renderPanel("x", 4, 1, "title"), "\n") {
		if got := lipgloss.Width(line); got != 4 {
			t.Fatalf("narrow panel line is %d cells, want 4: %q", got, line)
		}
	}
}

func TestHomeTableKeepsNetworksVisible(t *testing.T) {
	columns, rows := homeLayout(mockSummaries(), 118)
	foundNetworks := false
	for _, column := range columns {
		foundNetworks = foundNetworks || column.Title == "Networks"
	}
	if !foundNetworks {
		t.Fatalf("network column missing: %+v", columns)
	}
	if rows[2][0] != "◐ DNS missing" {
		t.Fatalf("stale status = %q", rows[2][0])
	}
}

func TestTopBarKeepsLongAccountLabels(t *testing.T) {
	account := "123456789012 (prod) · 014498740021 (staging) · demo"
	model := newTestApp([]ConnSummary{{Name: "corp", Account: account}}, RunOptions{
		Account: account,
		Profile: "default",
		Version: "v1.2.3",
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 30})
	view := ansiEscapePattern.ReplaceAllString(updated.(appModel).View(), "")
	if !strings.Contains(view, account) {
		t.Fatalf("top bar truncated the account label:\n%s", view)
	}
}

func TestTopBarAndHintsAreAlwaysOn(t *testing.T) {
	model := dashboardTestModel()
	for _, size := range [][2]int{{200, 30}, {160, 24}, {90, 20}} {
		updated, _ := model.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := ansiEscapePattern.ReplaceAllString(updated.(appModel).View(), "")
		rows := strings.Split(view, "\n")
		if !strings.Contains(rows[0], "BASTIONCTL") {
			t.Fatalf("size %v: top bar missing:\n%s", size, view)
		}
		if !strings.Contains(view, "<enter>") {
			t.Fatalf("size %v: header hint bar missing:\n%s", size, view)
		}
	}
}

func TestNoticeRendersAsBorderedPanel(t *testing.T) {
	model := newTestApp(nil, RunOptions{})
	model.setError("boom")
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	view := ansiEscapePattern.ReplaceAllString(updated.(appModel).View(), "")
	for _, expected := range []string{"notice", "5s · boom", "┌", "└"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("notice panel missing %q:\n%s", expected, view)
		}
	}
}

func TestOverlayCenterPreservesBackgroundSides(t *testing.T) {
	border := "│" + strings.Repeat(" ", 38) + "│"
	base := strings.Join([]string{border, border, border, border, border}, "\n")
	panel := "┌──┐\n│hi│\n└──┘"
	view := ansiEscapePattern.ReplaceAllString(overlayCenter(base, panel, 40), "")
	for _, line := range strings.Split(view, "\n") {
		if !strings.HasPrefix(line, "│") || !strings.HasSuffix(line, "│") {
			t.Fatalf("overlay erased the background border: %q", line)
		}
	}
}
