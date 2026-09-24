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

func TestRouteModelTogglesSelection(t *testing.T) {
	model := routeModel{routes: []config.NetworkRoute{{CIDR: "10.0.0.0/16"}}}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !updated.(routeModel).routes[0].Selected {
		t.Fatal("route was not selected")
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

func TestConnectionTableRendersStatusAndSelectsRow(t *testing.T) {
	choices := []ConnectionChoice{
		{Status: "[Running:123]", Name: "analytics", Kind: "vpc", Region: "eu-central-1", Networks: "10.0.0.0/16"},
		{Status: "[Not running]", Name: "operations", Kind: "dedicated", Region: "eu-west-1", Networks: "10.20.0.0/16"},
	}
	model := newConnectionModel(choices)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 14})
	model = updated.(connectionModel)
	view := model.View()
	for _, expected := range []string{"Status", "Connection", "[Running:123]", "analytics", "eu-central-1"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("connection table does not contain %q: %q", expected, view)
		}
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	updated, command := updated.(connectionModel).Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(connectionModel)
	if result.selected != 1 || command == nil {
		t.Fatalf("selected = %d, command nil = %v", result.selected, command == nil)
	}
}
