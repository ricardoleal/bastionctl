// chrome.go renders the persistent frame shared by every bastionctl screen:
// an information header on top, a key-hint bar at the bottom, and the budget
// math that keeps screen bodies inside the terminal height (k9s-style).
package ui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// maxInt is the "no cap" sentinel for body budgets.
const maxInt = math.MaxInt

var (
	chromeAppStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	chromeCellStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	chromeDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	chromeRuleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	chromeKeyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	chromeErrStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	chromeCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	helpBorderStyle   = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("6")).
				Padding(0, 2) // horizontal padding only: rows stay = content rows
)

// Hint is one footer keybinding: key letter plus what it does.
type Hint struct {
	Key   string // e.g. "s", "enter", "esc", ":"
	Label string // e.g. "start tunnel"
}

// renderHints formats the footer key bar: "s start  x stop  l logs".
func renderHints(hints []Hint, width int) string {
	if width <= 0 {
		width = 80
	}
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, chromeKeyStyle.Render(hint.Key)+chromeDimStyle.Render(" "+hint.Label))
	}
	line := strings.Join(parts, "  ")
	if lipgloss.Width(line) > width {
		// Drop hints from the front-end tail until it fits; keep the first
		// (usually the most important) and preserve order of the rest.
		for lipgloss.Width(line) > width && len(parts) > 1 {
			parts = parts[:len(parts)-1]
			line = strings.Join(parts, "  ")
		}
	}
	return line
}

// Chrome carries the per-screen frame content.
//
// Title  — left side of the header (app name / screen context).
// Info   — right side of the header (counts, status summary).
// Hints  — key hint bar shown when normal mode is active.
// Error  — one-line error message that replaces/extends the footer for a beat.
// Shell  — non-empty when the `:` command line is open; rendered as the
//
//	bottom-most line with a static command suggestion list above it.
type Chrome struct {
	Title            string
	Info             string
	Hints            []Hint
	Error            string
	SearchOpen       bool
	Search           string
	ShellOpen        bool
	Shell            string
	ShellSuggestions []string
}

// chromeRule returns a horizontal line of width w.
func chromeRule(w int) string {
	if w < 1 {
		w = 1
	}
	return chromeRuleStyle.Render(strings.Repeat("─", w))
}

// chromeHeight is how many body lines the chrome consumes:
// header, rule, rule, and one footer line (hints); the shell command
// line and both the error line add one extra row each.
func chromeHeight(c Chrome) int {
	height := 4
	if c.Error != "" {
		height++
	}
	if c.SearchOpen || c.ShellOpen {
		height++
	}
	return height
}

// Budget returns how many lines the body may use on this screen in
// terminal height h (h <= 0 disables the cap, e.g. for tests).
func (c Chrome) Budget(h int) int {
	if h <= 0 {
		return maxInt
	}
	budget := h - chromeHeight(c)
	if budget < 1 {
		budget = 1
	}
	return budget
}

// View renders the full frame around a body: header, rule, body, rule,
// footer. Body is expected to already be budgeted for the terminal height.
func (c Chrome) View(body string, width, height int) string {
	var b strings.Builder
	if height > 0 {
		budget := c.Budget(height)
		bodyLines := 1
		if body != "" {
			bodyLines = strings.Count(body, "\n") + 1
		}
		if bodyLines < budget {
			body += strings.Repeat("\n", budget-bodyLines)
		}
	}

	// Header: title left, info right.
	left := chromeAppStyle.Render(c.Title)
	if c.Info != "" {
		right := chromeCellStyle.Render(c.Info)
		gap := width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			// Too narrow to show both: keep the title only.
			b.WriteString(left)
		} else {
			fmt.Fprintf(&b, "%s%s%s", left, strings.Repeat(" ", gap), right)
		}
	} else {
		b.WriteString(left)
	}
	b.WriteString("\n")
	b.WriteString(chromeRule(width))
	b.WriteString("\n")
	b.WriteString(body)
	b.WriteString("\n")
	b.WriteString(chromeRule(width))
	b.WriteString("\n")

	switch {
	case c.SearchOpen:
		b.WriteString(chromeDimStyle.Render("type to filter · enter apply · esc cancel"))
		b.WriteString("\n")
		b.WriteString(chromeKeyStyle.Render("/"))
		b.WriteString(c.Search)
		b.WriteString(chromeCursorStyle.Render("▏"))
	case c.ShellOpen:
		// While the command line is open it owns the footer; an error
		// line (from the last submission) replaces the suggestion row.
		if c.Error != "" {
			b.WriteString(chromeErrStyle.Render(truncate(c.Error, max(width-1, 1))))
		} else {
			b.WriteString(renderShellSuggestions(c.ShellSuggestions, width))
		}
		b.WriteString("\n")
		b.WriteString(chromeKeyStyle.Render(":"))
		b.WriteString(c.Shell)
		b.WriteString(chromeCursorStyle.Render("▏"))
	default:
		if c.Error != "" {
			b.WriteString(chromeErrStyle.Render(truncate(c.Error, max(width-1, 1))))
			b.WriteString("\n")
		}
		b.WriteString(renderHints(c.Hints, width))
	}
	return b.String()
}

// renderShellSuggestions shows the command names one per compact line while
// the `:` command line is open.
func renderShellSuggestions(commands []string, width int) string {
	if len(commands) == 0 {
		return chromeDimStyle.Render("type a command · tab complete · enter run · esc cancel")
	}
	line := strings.Join(commands, "  ")
	return chromeDimStyle.Render(truncate(line, max(width-1, 1)))
}

// overlayCenter composites a floating panel over the base body: each row of
// the panel is drawn centered on its own line, padded to the panel width so
// the base content is not visible behind the window.
func overlayCenter(base, panel string, width int) string {
	baseLines := strings.Split(base, "\n")
	panelLines := strings.Split(panel, "\n")
	panelWidth := 0
	for _, row := range panelLines {
		panelWidth = max(panelWidth, lipgloss.Width(row))
	}
	top := (len(baseLines) - len(panelLines)) / 2
	if top < 0 {
		top = 0
	}
	rows := max(len(baseLines), top+len(panelLines))
	out := make([]string, rows)
	for i := 0; i < rows; i++ {
		switch {
		case i < len(baseLines) && (i < top || i >= top+len(panelLines)):
			out[i] = baseLines[i]
		case i >= top && i < top+len(panelLines):
			left := (width - panelWidth) / 2
			if left < 0 {
				left = 0
			}
			out[i] = strings.Repeat(" ", left) + panelLines[i-top]
		default:
			out[i] = ""
		}
	}
	return strings.Join(out, "\n")
}

// Dot returns a colored status glyph for the four connection states.
// The glyphs differ by shape, not only color, to stay readable without color.
func Dot(state string) string {
	switch state {
	case "ok":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("●")
	case "warn":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("◐")
	case "fail":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("●")
	default:
		return chromeDimStyle.Render("○")
	}
}
