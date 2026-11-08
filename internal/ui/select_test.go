package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"bastionctl/internal/config"
	"bastionctl/internal/probe"
)

func TestSelectModelKeyboardSelection(t *testing.T) {
	model := selectModel{choices: []Choice{{Title: "one"}, {Title: "two"}}, selected: -1}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, command := updated.(selectModel).Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(selectModel)
	if result.selected != 1 || command == nil {
		t.Fatalf("selected = %d, command nil = %v", result.selected, command == nil)
	}
}

func TestCandidateSelectionMarksCurrentPivot(t *testing.T) {
	candidate := probe.Candidate{Name: "existing", InstanceID: "i-current", Current: true, Reachable: true}
	if title := candidateTitle(candidate); !strings.Contains(title, "[current]") {
		t.Fatalf("candidate title does not mark current pivot: %q", title)
	}
	rows := candidateRows([]probe.Candidate{candidate}, 120)
	if !strings.Contains(rows[0][0], "[current]") {
		t.Fatalf("candidate row does not mark current pivot: %#v", rows[0])
	}
}

func TestRouteModelTogglesSelection(t *testing.T) {
	model := routeModel{routes: []config.NetworkRoute{{CIDR: "10.0.0.0/16"}}}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !updated.(routeModel).routes[0].Selected {
		t.Fatal("route was not selected")
	}
}

func TestLineWindowFitsBudgetAndKeepsCursorVisible(t *testing.T) {
	// Eight items: heights 1 for odd, 2 for even? Actually 1,2 alternating.
	heights := []int{1, 2, 1, 2, 1, 2, 1, 2}

	// Unlimited budget shows everything.
	if start, end := lineWindow(heights, 3, 0, listBudget(0)); start != 0 || end != 8 {
		t.Fatalf("unlimited: %d..%d", start, end)
	}

	// Terminal height 10 -> 6 item lines. Cursor 7 (2 lines) must be visible;
	// filling downward from the cursor takes item indexes 7 only (2 lines),
	// then pushes items 6, 5, 4 above it.
	start, end := lineWindow(heights, 7, 0, listBudget(10))
	if !(start <= 7 && 7 < end) {
		t.Fatalf("cursor 7 not visible: %d..%d", start, end)
	}
	used := 0
	for index := start; index < end; index++ {
		used += heights[index]
	}
	if used > listBudget(10) {
		t.Fatalf("window uses %d lines over budget %d", used, listBudget(10))
	}

	// Scrolling upward with a stale low anchor must still reveal the cursor.
	if start, end = lineWindow(heights, 0, 5, listBudget(10)); start != 0 || !(0 < end) {
		t.Fatalf("jump-to-top: %d..%d", start, end)
	}

	// Tiny budget always shows exactly the cursor item.
	if start, end = lineWindow(heights, 5, 0, 2); start != 5 || end != 6 {
		t.Fatalf("tiny budget: %d..%d", start, end)
	}
}

func TestSelectModelViewSlicesToTerminalHeight(t *testing.T) {
	choices := make([]Choice, 40)
	for index := range choices {
		choices[index] = Choice{Title: fmt.Sprintf("choice-%02d", index)}
	}
	model := selectModel{title: "pick", choices: choices, cursor: 0, selected: -1}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	model = updated.(selectModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	model = updated.(selectModel)

	view := model.View()
	if !strings.Contains(view, "choice-06") {
		t.Fatalf("pgdn should land on item 6, view: %q", view)
	}
	if strings.Contains(view, "choice-39") {
		t.Fatalf("items beyond the window must not render: %q", view)
	}
	if !strings.Contains(view, "7/40") {
		t.Fatalf("position indicator missing: %q", view)
	}
}

func TestRouteModelViewScrollsLongNetworkLists(t *testing.T) {
	routes := make([]config.NetworkRoute, 30)
	for index := range routes {
		routes[index] = config.NetworkRoute{CIDR: fmt.Sprintf("10.%d.0.0/16", index), Source: "pivot_vpc"}
	}
	model := routeModel{routes: routes}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	model = updated.(routeModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, _ = updated.(routeModel).Update(tea.KeyMsg{Type: tea.KeyPgDown})
	model = updated.(routeModel)

	view := model.View()
	if strings.Count(view, "\n") > 12+1 {
		t.Fatalf("view exceeds terminal height: %d lines", strings.Count(view, "\n"))
	}
	// pgdn from 1 by page size (12-4 reserved = 8 lines budget / 2 per item = 4)
	// should land on row 5.
	if model.cursor != 5 {
		t.Fatalf("cursor = %d, want 5", model.cursor)
	}
	if !strings.Contains(view, "10.5.0.0/16") {
		t.Fatalf("cursor route not visible: %q", view)
	}
}

func TestCandidateTableScrollsWithSelection(t *testing.T) {
	candidates := make([]probe.Candidate, 49)
	for index := range candidates {
		candidates[index] = probe.Candidate{
			Name:       fmt.Sprintf("candidate-%02d", index),
			InstanceID: fmt.Sprintf("i-%017d", index),
			VpcID:      "vpc-123",
			VpcCIDRs:   []string{"10.0.0.0/16"},
			Method:     probe.MethodSSM,
			Reachable:  true,
		}
	}
	model := newCandidateModel(candidates)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 14})
	model = updated.(candidateModel)
	for range 20 {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = updated.(candidateModel)
	}
	if model.table.Cursor() != 20 {
		t.Fatalf("cursor = %d, want 20", model.table.Cursor())
	}
	view := model.View()
	if !strings.Contains(view, "candidate-20") || !strings.Contains(view, "21/49") {
		t.Fatalf("selected candidate is not visible: %q", view)
	}
}

func TestCandidateTableResizeAcrossColumnBuckets(t *testing.T) {
	candidates := make([]probe.Candidate, 9)
	for index := range candidates {
		candidates[index] = probe.Candidate{
			Name:       "pivot",
			InstanceID: "i-123",
			VpcID:      "vpc-123",
			VpcCIDRs:   []string{"10.0.0.0/16"},
			Method:     probe.MethodSSM,
			Reachable:  true,
		}
	}
	// newCandidateModel starts at width 120 (5 columns). Resizing to a width
	// with more columns (6) or fewer (3) must not panic the bubbles table.
	model := newCandidateModel(candidates)
	for _, width := range []int{150, 70, 200, 120} {
		updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		model = updated.(candidateModel)
		_ = model.View()
	}
}

func TestHomeTableRendersStatusColumns(t *testing.T) {
	summaries := []ConnSummary{
		{Name: "analytics", Account: "123456789012", Kind: "vpc", Method: "ssm", Region: "eu-central-1", Networks: "10.0.0.0/16", DNS: "off", Running: true, PID: 123},
		{Name: "operations", Account: "123456789012", Kind: "dedicated", Method: "ssh", Region: "eu-west-1", Networks: "10.20.0.0/16", DNS: "off"},
	}
	// Resizing across column tiers must not panic the bubbles table.
	model := newHomeTable(summaries, 0)
	for _, width := range []int{160, 120, 100, 70, 200, 120} {
		model, _ = model.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		_ = model.View()
	}
	_, rows := homeLayout(summaries, 160)
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0][1] != "analytics" || rows[0][2] != "eu-central-1" || rows[0][6] != "123456789012" {
		t.Fatalf("row = %#v", rows[0])
	}
}
