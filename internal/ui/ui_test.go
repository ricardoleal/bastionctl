package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTruncateMeasuresStyledTextByDisplayWidth(t *testing.T) {
	styled := "\x1b[2m" + strings.Repeat("x", 100) + "\x1b[0m"
	got := truncate(styled, 10)

	if width := ansi.StringWidth(got); width != 10 {
		t.Fatalf("display width = %d, want 10: %q", width, got)
	}
	if !strings.Contains(got, "\u2026") {
		t.Fatalf("truncated text missing ellipsis: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("truncated text dropped the ANSI reset: %q", got)
	}
}

func TestTruncateLeavesFittingTextUntouched(t *testing.T) {
	styled := "\x1b[1;92mok\x1b[0m"
	if got := truncate(styled, 2); got != styled {
		t.Fatalf("text should not be truncated: %q", got)
	}
}

func TestTruncateHandlesSingleCellBudget(t *testing.T) {
	if got := truncate("abcdef", 1); got != "a" {
		t.Fatalf("truncate(..., 1) = %q, want %q", got, "a")
	}
}

func TestHelpPanelLinesFitNarrowTerminals(t *testing.T) {
	for _, width := range []int{120, 80, 60, 30} {
		inner := width - 6
		switch {
		case inner > 78:
			inner = 78
		case inner < 24:
			inner = 24
		}
		for _, line := range helpPanelLines(width) {
			if got := ansi.StringWidth(line); got > inner {
				t.Fatalf("width %d: line uses %d cells (max %d): %q", width, got, inner, line)
			}
		}
	}
}
