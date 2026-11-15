// chrome.go renders the shared header, prompts, notices, and screen budgets.
package ui

import (
	"math"
	"os"
	"strings"

	"bastionctl/internal/project"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// maxInt is the "no cap" sentinel for body budgets.
const maxInt = math.MaxInt

var (
	chromeCellStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	chromeDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	chromeKeyStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	chromeErrStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	chromeCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	noticeBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	noticeTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	helpBorderStyle   = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("6")).
				Padding(0, 2) // horizontal padding only: rows stay = content rows
)

// Hint is one contextual keybinding: key letter plus what it does.
type Hint struct {
	Key   string // e.g. "s", "enter", "esc", ":"
	Label string // e.g. "start tunnel"
}

// Chrome carries the per-screen frame content.
//
// Title is shown for standalone selectors; Info holds status text. Dashboard
// holds home metadata, Hints are contextual shortcuts, and Error is a notice.
// Search and Shell control the prompt immediately above the active panel.
type Chrome struct {
	Title            string
	ShowTitle        bool
	MinHeaderRows    int
	Info             string
	Dashboard        []string
	Hints            []Hint
	Error            string
	Loading          string
	SearchOpen       bool
	Search           string
	ShellOpen        bool
	Shell            string
	ShellPrefix      string
	ShellSuggestions []string
}

// chromeHeight counts header, prompt, and footer rows outside the panel.
func chromeHeight(c Chrome, width, terminalHeight int) int {
	height := len(c.headerRows(width, terminalHeight)) + 1
	if c.ShellOpen {
		return height + 1
	}
	if c.SearchOpen {
		height++
	}
	return height
}

// Budget returns how many lines the body may use on this screen in
// terminal height h (h <= 0 disables the cap, e.g. for tests).
func (c Chrome) Budget(h int, widths ...int) int {
	if h <= 0 {
		return maxInt
	}
	width := 80
	if len(widths) > 0 && widths[0] > 0 {
		width = widths[0]
	}
	budget := h - chromeHeight(c, width, h)
	if budget < 1 {
		budget = 1
	}
	return budget
}

// View renders the shared frame around a body already budgeted for the height.
func (c Chrome) View(body string, width, height int) string {
	var b strings.Builder
	if height > 0 {
		budget := c.Budget(height, width)
		bodyLines := 1
		if body != "" {
			bodyLines = strings.Count(body, "\n") + 1
		}
		if bodyLines < budget {
			body += strings.Repeat("\n", budget-bodyLines)
		}
	}

	for _, line := range c.headerRows(width, height) {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if c.ShellOpen {
		prefix := c.ShellPrefix
		if prefix == "" {
			prefix = ":"
		}
		b.WriteString(renderCommandPrompt(prefix, c.Shell, c.ShellSuggestions, width))
		b.WriteString("\n")
	} else if c.SearchOpen {
		b.WriteString(renderPrompt("/", c.Search, width))
		b.WriteString("\n")
	}
	if c.Error != "" {
		body = overlayNotice(body, c.Error, width)
	} else if c.Loading != "" && !c.ShellOpen {
		body = overlayNotice(body, c.Loading, width)
	}
	b.WriteString(body)
	b.WriteString("\n")
	mark := "\uf408"
	if os.Getenv("BASTIONCTL_NO_ICONS") != "" || (os.Getenv("TERM_PROGRAM") == "iTerm.app" && os.Getenv("BASTIONCTL_ICONS") != "1") {
		mark = "gh"
	}
	label := project.Repository
	if width > 0 && lipgloss.Width(mark+" "+label) > width {
		label = "bastionctl"
		if lipgloss.Width(mark+" "+label) > width {
			label = ""
		}
	}
	footer := chromeDimStyle.Render(mark)
	if label != "" {
		footer += " " + chromeDimStyle.Render(label)
	}
	b.WriteString(rightAlignCell(footer, width))
	return b.String()
}

func (c Chrome) headerRows(width, height int) []string {
	if width <= 0 {
		width = 80
	}
	if width < 90 && len(c.Dashboard) == 0 {
		rows := []string{c.topBar(width)}
		for _, line := range c.hintRows(width, height) {
			rows = append(rows, rightAlignCell(line, width))
		}
		rows = append(rows, c.Dashboard...)
		for len(rows) < c.MinHeaderRows {
			rows = append(rows, "")
		}
		return rows
	}
	leftWidth := max(width/3, 26)
	if len(c.Dashboard) == 0 {
		leftWidth = min(max(lipgloss.Width(c.Info), 16), width/3)
	}
	for _, line := range c.Dashboard {
		leftWidth = max(leftWidth, lipgloss.Width(line))
	}
	leftWidth = min(leftWidth, width*5/12)
	if width < 110 {
		leftWidth = min(leftWidth, max(width*3/8, 26))
	}
	logo := []string{
		dashboardLogoStyle.Render(dashboardLogoWord),
		dashboardLogoStyle.Render("  ╔═╦═╗"),
		dashboardLogoStyle.Render("  ╚═╬═╝"),
	}
	largeLogo := width >= 110 && len(c.Dashboard) >= 7 && (height <= 0 || height >= 20)
	if largeLogo {
		logo = []string{
			dashboardLogoStyle.Render("      BASTIONCTL"),
			dashboardLogoStyle.Render("   ╔═╗ ╔═╗ ╔═╗ ╔═╗"),
			dashboardLogoStyle.Render("   ║ ╚═╝ ╚═╝ ╚═╝ ║"),
			dashboardLogoStyle.Render("   ║    ╔═══╗    ║"),
			dashboardLogoStyle.Render("   ║    ║ ╬ ║    ║"),
			dashboardLogoStyle.Render("   ╚════╣ ║ ╠════╝"),
			dashboardLogoStyle.Render("        ╚═╩═╝"),
		}
	}
	if width < 110 || height > 0 && height < 12 {
		logo = []string{dashboardLogoStyle.Render(dashboardLogoWord)}
	}
	if width < 75 {
		logo = []string{dashboardLogoStyle.Render("▣")}
	}
	logoWidth := 0
	for _, line := range logo {
		logoWidth = max(logoWidth, lipgloss.Width(line))
	}
	hintWidth := max(width-leftWidth-logoWidth-6, 1)
	maxRows := max(len(c.Hints), 1)
	if height > 0 {
		maxRows = max(height-7, 1)
	}
	columns, rowsNeeded := 1, min(max(len(c.Hints), 1), maxRows)
	keyWidths, columnWidths := []int{1}, []int{hintWidth}
	for _, hint := range c.Hints {
		keyWidths[0] = max(keyWidths[0], min(lipgloss.Width("<"+hint.Key+">"), hintWidth))
	}
	usedWidth := hintWidth
	for candidate := min(max(len(c.Hints), 1), max(hintWidth/6, 1)); candidate > 0; candidate-- {
		candidateRows := max((len(c.Hints)+candidate-1)/candidate, 1)
		if candidateRows > maxRows {
			continue
		}
		keys := make([]int, candidate)
		labels := make([]int, candidate)
		for index, hint := range c.Hints {
			column := index / candidateRows
			keys[column] = max(keys[column], lipgloss.Width("<"+hint.Key+">"))
			labels[column] = max(labels[column], lipgloss.Width(hint.Label))
		}
		widths := make([]int, candidate)
		total := 0
		for column := range widths {
			widths[column] = keys[column] + labels[column] + 3
			total += widths[column]
		}
		if total <= hintWidth {
			columns, rowsNeeded = candidate, candidateRows
			keyWidths, columnWidths, usedWidth = keys, widths, total
			break
		}
	}
	spacing := max((hintWidth-usedWidth)/columns, 0)
	left := c.Dashboard
	if len(left) == 0 {
		left = []string{chromeCellStyle.Render(c.Info)}
	}
	rowCount := max(len(left), rowsNeeded, len(logo), c.MinHeaderRows)
	if !largeLogo && width >= 75 && rowCount >= 7 {
		logo = []string{
			dashboardLogoStyle.Render(dashboardLogoWord),
			dashboardLogoStyle.Render(" ╔╦═╦═╦╗"),
			dashboardLogoStyle.Render(" ║     ║"),
			dashboardLogoStyle.Render(" ║ ╔═╗ ║"),
			dashboardLogoStyle.Render(" ║ ║ ║ ║"),
			dashboardLogoStyle.Render(" ╚═╝ ╚═╝"),
			dashboardLogoStyle.Render("    ║"),
		}
	} else if !largeLogo && width >= 75 && rowCount >= 5 {
		logo = []string{
			dashboardLogoStyle.Render(dashboardLogoWord),
			dashboardLogoStyle.Render("  ╔═╦═╗"),
			dashboardLogoStyle.Render("  ║╔═╗║"),
			dashboardLogoStyle.Render("  ║║ ║║"),
			dashboardLogoStyle.Render("  ╚╩═╩╝"),
		}
	}
	rows := make([]string, rowCount)
	for row := range rows {
		var line strings.Builder
		if row < len(left) {
			line.WriteString(padCell(left[row], leftWidth))
		} else {
			line.WriteString(strings.Repeat(" ", leftWidth))
		}
		line.WriteString("  ")
		for column := 0; column < columns; column++ {
			index := column*rowsNeeded + row
			if column == columns-1 && row == rowsNeeded-1 && len(c.Hints) > columns*rowsNeeded {
				index = len(c.Hints) - 1
			}
			item := ""
			if row < rowsNeeded && index < len(c.Hints) {
				hint := c.Hints[index]
				item = padCell(dashboardKeyStyle.Render("<"+hint.Key+">"), keyWidths[column]) + " " + dashboardKeyLabelStyle.Render(hint.Label)
			}
			line.WriteString(padCell(item, columnWidths[column]+spacing))
		}
		used := leftWidth + 2 + usedWidth + columns*spacing
		if row < len(logo) && width-used >= logoWidth+2 {
			line.WriteString(strings.Repeat(" ", width-used-logoWidth-2))
			line.WriteString(padCell(logo[row], logoWidth))
			line.WriteString("  ")
		}
		rows[row] = line.String()
	}
	return rows
}

// topBar places status on the left and the compact wordmark on the right.
func (c Chrome) topBar(width int) string {
	if width <= 0 {
		width = 80
	}
	left := chromeCellStyle.Render(c.Info)
	if c.ShowTitle && c.Title != "" {
		left = chromeCellStyle.Render(c.Title)
		if c.Info != "" {
			left += chromeDimStyle.Render(" · " + c.Info)
		}
	}
	brand := dashboardLogoStyle.Render("▣ " + dashboardLogoWord)
	if lipgloss.Width(left)+lipgloss.Width(brand)+3 > width {
		return truncate(left, width)
	}
	return left + strings.Repeat(" ", width-lipgloss.Width(left)-lipgloss.Width(brand)-2) + brand + "  "
}

// renderHintBar fits contextual hints in one row, preserving the final hint.
func renderHintBar(hints []Hint, width int) string {
	if width <= 0 {
		width = 80
	}
	if len(hints) == 0 {
		return ""
	}
	rendered := make([]string, len(hints))
	for index, hint := range hints {
		rendered[index] = dashboardKeyStyle.Render("<"+hint.Key+">") +
			" " + dashboardKeyLabelStyle.Render(hint.Label)
	}
	for len(rendered) > 1 {
		line := strings.Join(rendered, "  ")
		if lipgloss.Width(line) <= width-1 {
			return line
		}
		// Drop the second-to-last hint so the last one survives.
		rendered = append(rendered[:len(rendered)-2], rendered[len(rendered)-1])
	}
	return truncate(rendered[0], max(width-1, 1))
}

func (c Chrome) hintRows(width, height int) []string {
	if width <= 0 {
		width = 80
	}
	limit := 4
	if height > 0 {
		limit = max(min(limit, height-len(c.Dashboard)-6), 1)
	}
	rows := []string{""}
	rowHints := []Hint{}
	for _, hint := range c.Hints {
		item := dashboardKeyStyle.Render("<"+hint.Key+">") + " " + dashboardKeyLabelStyle.Render(hint.Label)
		last := len(rows) - 1
		separator := ""
		if rows[last] != "" {
			separator = "  "
		}
		if lipgloss.Width(rows[last])+len(separator)+lipgloss.Width(item) > width && rows[last] != "" {
			if len(rows) == limit {
				final := c.Hints[len(c.Hints)-1]
				rows[last] = renderHintBar(append(rowHints, final), width)
				break
			}
			rows = append(rows, "")
			rowHints = nil
			last++
			separator = ""
		}
		rows[last] += separator + truncate(item, width)
		rowHints = append(rowHints, hint)
	}
	return rows
}

// renderNoticePanel draws a transient notice for overlaying the panel.
func renderNoticePanel(message string, width int) string {
	if width <= 0 {
		width = 80
	}
	inner := min(max(lipgloss.Width(message)+4, 30), max(width-2, 12))
	title := "notice"
	top := noticeBorderStyle.Render("┌─ ") +
		noticeTitleStyle.Render(title) +
		noticeBorderStyle.Render(" "+strings.Repeat("─", max(inner-lipgloss.Width(title)-3, 0))+"┐")
	body := noticeBorderStyle.Render("│") +
		padCell(" "+truncate(message, inner-2)+" ", inner) +
		noticeBorderStyle.Render("│")
	bottom := noticeBorderStyle.Render("└" + strings.Repeat("─", inner) + "┘")
	return strings.Join([]string{top, body, bottom}, "\n")
}

func overlayNotice(base, message string, width int) string {
	if width <= 0 {
		width = 80
	}
	rows := strings.Split(base, "\n")
	panel := strings.Split(renderNoticePanel(message, width), "\n")
	if len(rows) < len(panel)+2 {
		panel = []string{chromeErrStyle.Render(truncate(message, width))}
	}
	panelWidth := 0
	for _, line := range panel {
		panelWidth = max(panelWidth, lipgloss.Width(line))
	}
	top := max(len(rows)-len(panel)-1, 0)
	left := max(width-panelWidth-1, 0)
	for index, line := range panel {
		rows[top+index] = overlayRow(rows[top+index], line, left, panelWidth, width)
	}
	return strings.Join(rows, "\n")
}

// renderPrompt keeps the active input and cursor within the terminal width.
func renderPrompt(prefix, input string, width int) string {
	if width <= 0 {
		width = 80
	}
	if width == 1 {
		return chromeCursorStyle.Render("|")
	}
	available := max(width-lipgloss.Width(prefix)-1, 0)
	characters := []rune(input)
	start := len(characters)
	for start > 0 && lipgloss.Width(string(characters[start-1:])) <= available {
		start--
	}
	return chromeKeyStyle.Render(truncate(prefix, max(width-1, 0))) + chromeCellStyle.Render(string(characters[start:])) + chromeCursorStyle.Render("|")
}

func renderCommandPrompt(prefix, input string, suggestions []string, width int) string {
	if width <= 0 {
		width = 80
	}
	prompt := renderPrompt(prefix, input, width)
	available := width - lipgloss.Width(prompt)
	if available <= 0 {
		return prompt
	}
	for _, suggestion := range suggestions {
		if strings.HasPrefix(strings.ToLower(suggestion), strings.ToLower(input)) {
			characters := []rune(suggestion)
			inputLength := len([]rune(input))
			if inputLength < len(characters) {
				return prompt + chromeDimStyle.Render(ansi.Truncate(string(characters[inputLength:]), available, ""))
			}
			break
		}
	}
	return prompt
}

// rightAlignCell pushes cell to the right edge of a width-wide line.
func rightAlignCell(cell string, width int) string {
	if width <= 0 {
		width = 80
	}
	cell = truncate(cell, width)
	gap := width - lipgloss.Width(cell)
	if gap <= 0 {
		return cell
	}
	return strings.Repeat(" ", gap) + cell
}

// overlayCenter composites a floating panel over the base body: each row of
// the panel is drawn centered on its own line. The base content to the left
// and right of the panel is preserved, so panel borders and table columns are
// not erased around the popup.
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
			left := max((width-panelWidth)/2, 0)
			underlay := ""
			if i < len(baseLines) {
				underlay = baseLines[i]
			}
			out[i] = overlayRow(underlay, panelLines[i-top], left, panelWidth, width)
		default:
			out[i] = ""
		}
	}
	return strings.Join(out, "\n")
}

// overlayRow draws panel over base at the given left offset, keeping the base
// cells before and after the panel. It is ANSI-aware and pads short base rows
// so the panel still lands at the requested column.
func overlayRow(base, panel string, left, panelWidth, width int) string {
	if width <= 0 {
		width = left + panelWidth
	}
	baseWidth := lipgloss.Width(base)
	if baseWidth < left {
		base += strings.Repeat(" ", left-baseWidth)
		baseWidth = left
	}
	prefix := ansi.Cut(base, 0, left)
	right := left + panelWidth
	suffix := ""
	if right < baseWidth {
		suffix = ansi.Cut(base, right, min(baseWidth, max(width, right)))
	}
	return prefix + panel + suffix
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
