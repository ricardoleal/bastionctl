package ui

import (
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"bastionctl/internal/config"
	"bastionctl/internal/probe"
)

type Choice struct {
	Title       string
	Description string
}

type selectModel struct {
	title    string
	choices  []Choice
	cursor   int
	start    int // first visible item index (scroll anchor)
	height   int // terminal height; 0 = unlimited (tests)
	width    int
	selected int
	canceled bool
}

var (
	titleStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	selectedStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	descriptionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	warningStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// listBudget keeps the legacy four-line reserve used by the scrollable
// pickers (header, rule, rule, footer — see Chrome).
func listBudget(height int) int {
	return Chrome{}.Budget(height)
}

// lineWindow slices [start,end) item indexes that fit budget lines and
// always contain cursor. start is the preferred upper anchor and is adjusted
// minimally, so navigation scrolls the by-one and keeps long lists stable.
func lineWindow(heights []int, cursor, start, budget int) (int, int) {
	count := len(heights)
	if count == 0 {
		return 0, 0
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= count {
		cursor = count - 1
	}
	if bucket := heights[cursor]; budget < bucket {
		budget = bucket
	}
	if cursor < start {
		start = cursor
	}
	if start > cursor {
		start = cursor
	}
	if start < 0 {
		start = 0
	}
	// Shrink from the top until start..cursor fits the budget.
	used := 0
	for index := start; index <= cursor; index++ {
		used += heights[index]
	}
	for used > budget && start < cursor {
		used -= heights[start]
		start++
	}
	// Fill downward from the cursor...
	end := cursor + 1
	for end < count && used+heights[end] <= budget {
		used += heights[end]
		end++
	}
	// ...then fill upward above the retained top anchor.
	for start > 0 && used+heights[start-1] <= budget {
		start--
		used += heights[start]
	}
	return start, end
}

func (m selectModel) Init() tea.Cmd { return nil }

// selectItemHeights returns the rendered line count of each choice: one line
// for the title, plus one when a description is shown under it.
func selectItemHeights(choices []Choice) []int {
	heights := make([]int, len(choices))
	for index, choice := range choices {
		if choice.Description == "" {
			heights[index] = 1
		} else {
			heights[index] = 2
		}
	}
	return heights
}

func (m selectModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.height = message.Height
		m.width = message.Width
		return m, nil
	case tea.KeyMsg:
		switch message.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}
		case "pgup":
			m.cursor -= m.pageStep()
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "pgdown":
			m.cursor += m.pageStep()
			if m.cursor > len(m.choices)-1 {
				m.cursor = len(m.choices) - 1
			}
		case "enter":
			m.selected = m.cursor
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.canceled = true
			return m, tea.Quit
		default:
			heights := selectItemHeights(m.choices)
			m.start, _ = lineWindow(heights, m.cursor, m.start, listBudget(m.height))
			return m, nil
		}
	}
	heights := selectItemHeights(m.choices)
	m.start, _ = lineWindow(heights, m.cursor, m.start, listBudget(m.height))
	return m, nil
}

// pageStep is how far pgup/pgdn jump: the currently visible item count.
func (m selectModel) pageStep() int {
	budget := listBudget(m.height)
	if budget == math.MaxInt {
		return len(m.choices)
	}
	count := 0
	used := 0
	for _, height := range selectItemHeights(m.choices) {
		if used+height > budget {
			break
		}
		used += height
		count++
	}
	if count < 1 {
		count = 1
	}
	return count
}

func (m selectModel) chrome() Chrome {
	return Chrome{
		Title: m.title,
		Info:  fmt.Sprintf("%d/%d", m.cursor+1, len(m.choices)),
		Hints: []Hint{
			{"↑/↓", "move"},
			{"pgup/pgdn", "page"},
			{"enter", "select"},
			{"q", "cancel"},
		},
	}
}

func (m selectModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	heights := selectItemHeights(m.choices)
	if len(heights) > 0 {
		start, end := lineWindow(heights, m.cursor, m.start, m.chrome().Budget(m.height))
		b.WriteString(renderChoices(m.choices, m.cursor, start, end))
		b.WriteString("\n")
	}
	return m.chrome().View(b.String(), width, m.height)
}

// renderChoices draws the items in [start,end) with the cursor highlighted.
// Line counts must stay consistent with selectItemHeights.
func renderChoices(choices []Choice, cursor, start, end int) string {
	var b strings.Builder
	for index := start; index < end; index++ {
		marker := "  "
		title := choices[index].Title
		if index == cursor {
			marker = "> "
			title = selectedStyle.Render(title)
		}
		fmt.Fprintf(&b, "%s%s\n", marker, title)
		if choices[index].Description != "" {
			fmt.Fprintf(&b, "    %s\n", descriptionStyle.Render(choices[index].Description))
		}
	}
	trimmed := b.String()
	if strings.HasSuffix(trimmed, "\n") {
		trimmed = strings.TrimSuffix(trimmed, "\n")
	}
	return trimmed
}

func Select(title string, choices []Choice) (int, bool, error) {
	if len(choices) == 0 {
		return 0, true, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Println(title)
		for index, choice := range choices {
			fmt.Printf("  %d. %s", index+1, choice.Title)
			if choice.Description != "" {
				fmt.Printf(" — %s", choice.Description)
			}
			fmt.Println()
		}
		return PromptSelect(len(choices), 1) - 1, false, nil
	}

	initial := selectModel{title: title, choices: choices, selected: -1}
	result, err := tea.NewProgram(initial, tea.WithAltScreen()).Run()
	if err != nil {
		return 0, false, err
	}
	model := result.(selectModel)
	return model.selected, model.canceled, nil
}

func sharedTableStyles() table.Styles {
	styles := table.DefaultStyles()
	styles.Header = styles.Header.Bold(true).Foreground(lipgloss.Color("6")).BorderStyle(lipgloss.NormalBorder()).BorderBottom(true)
	styles.Selected = styles.Selected.Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("2"))
	return styles
}

// safeSetColumnsRows updates a bubbles table's columns and rows without tripping
// the table's renderRow panic on mismatched widths.
//
// bubbles/table renderRow iterates over the row cells and indexes into columns,
// so any intermediate state where a row has more cells than there are columns
// panics (index out of range). Growing the column count therefore requires
// SetColumns before SetRows, while shrinking requires the reverse order.
func safeSetColumnsRows(t *table.Model, cols []table.Column, rows []table.Row) {
	if len(cols) >= len(t.Columns()) {
		t.SetColumns(cols)
		t.SetRows(rows)
		return
	}
	t.SetRows(rows)
	t.SetColumns(cols)
}

func SelectCandidate(candidates []probe.Candidate, currentInstanceID ...string) (int, bool, error) {
	if len(candidates) == 0 {
		return 0, true, nil
	}
	currentID := ""
	if len(currentInstanceID) > 0 {
		currentID = currentInstanceID[0]
	}
	candidates = append([]probe.Candidate(nil), candidates...)
	for index := range candidates {
		candidates[index].Current = candidates[index].InstanceID == currentID && currentID != ""
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		choices := make([]Choice, len(candidates))
		for index, candidate := range candidates {
			choices[index] = Choice{
				Title:       candidateTitle(candidate),
				Description: candidateDescription(candidate),
			}
		}
		title := "Select a pivot instance"
		if currentID != "" {
			title = "Select a new pivot instance (current: " + currentID + ")"
		}
		return Select(title, choices)
	}

	model := newCandidateModel(candidates)
	model.currentInstanceID = currentID
	result, err := tea.NewProgram(model, tea.WithAltScreen()).Run()
	if err != nil {
		return 0, false, err
	}
	selected := result.(candidateModel)
	return selected.selected, selected.canceled, nil
}

type candidateModel struct {
	candidates        []probe.Candidate
	table             table.Model
	width             int
	selected          int
	canceled          bool
	currentInstanceID string
}

func newCandidateModel(candidates []probe.Candidate) candidateModel {
	tableModel := table.New(
		table.WithColumns(candidateColumns(120)),
		table.WithRows(candidateRows(candidates, 120)),
		table.WithFocused(true),
		table.WithHeight(12),
		table.WithStyles(sharedTableStyles()),
	)
	return candidateModel{candidates: candidates, table: tableModel, width: 120, selected: -1}
}

func (m candidateModel) Init() tea.Cmd { return nil }

func (m candidateModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.table.SetWidth(max(message.Width, 40))
		m.table.SetHeight(max(Chrome{}.Budget(message.Height), 3))
		safeSetColumnsRows(&m.table, candidateColumns(message.Width), candidateRows(m.candidates, message.Width))
		return m, nil
	case tea.KeyMsg:
		switch message.String() {
		case "enter":
			m.selected = m.table.Cursor()
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.canceled = true
			return m, tea.Quit
		}
	}
	var command tea.Cmd
	m.table, command = m.table.Update(message)
	return m, command
}

func (m candidateModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	chrome := Chrome{
		Title: "Select a pivot instance",
		Info:  fmt.Sprintf("%d/%d · %d candidates", m.table.Cursor()+1, len(m.candidates), len(m.candidates)),
		Hints: []Hint{
			{"↑/↓", "move"},
			{"pgup/pgdn", "jump"},
			{"enter", "select"},
			{"q", "cancel"},
		},
	}
	if m.currentInstanceID != "" {
		chrome.Title = "Select a new pivot instance"
		chrome.Info += " · current: " + m.currentInstanceID
	}
	return chrome.View(m.table.View(), width, 0) // table height already capped in Update
}

func candidateColumns(width int) []table.Column {
	switch {
	case width >= 130:
		return []table.Column{
			{Title: "Name", Width: 32},
			{Title: "Instance", Width: 19},
			{Title: "Reachability", Width: 15},
			{Title: "Egress", Width: 11},
			{Title: "VPC", Width: 15},
			{Title: "Networks", Width: max(width-104, 20)},
		}
	case width >= 95:
		return []table.Column{
			{Title: "Name", Width: 25},
			{Title: "Instance", Width: 19},
			{Title: "Reachability", Width: 15},
			{Title: "Egress", Width: 11},
			{Title: "Networks", Width: max(width-82, 14)},
		}
	case width >= 75:
		return []table.Column{
			{Title: "Name", Width: max(width-59, 14)},
			{Title: "Instance", Width: 19},
			{Title: "Status", Width: 14},
			{Title: "Networks", Width: 18},
		}
	default:
		return []table.Column{
			{Title: "Name", Width: max(width-40, 12)},
			{Title: "Instance", Width: 19},
			{Title: "Status", Width: 14},
		}
	}
}

func candidateRows(candidates []probe.Candidate, width int) []table.Row {
	rows := make([]table.Row, len(candidates))
	for index, candidate := range candidates {
		name := candidate.Name
		if name == "" {
			name = "unnamed"
		}
		if candidate.Current {
			name += " [current]"
		}
		status := candidateStatus(candidate)
		egress := "-"
		if candidate.Reachable {
			egress = "restricted"
			if candidate.VpcEgressOK {
				egress = "OK"
			}
		}
		networks := strings.Join(candidate.VpcCIDRs, ", ")
		switch {
		case width >= 130:
			rows[index] = table.Row{name, candidate.InstanceID, status, egress, candidate.VpcID, networks}
		case width >= 95:
			rows[index] = table.Row{name, candidate.InstanceID, status, egress, networks}
		case width >= 75:
			rows[index] = table.Row{name, candidate.InstanceID, status, networks}
		default:
			rows[index] = table.Row{name, candidate.InstanceID, status}
		}
	}
	return rows
}

func candidateStatus(candidate probe.Candidate) string {
	switch {
	case candidate.Method == probe.MethodSSM:
		return "SSM online"
	case candidate.Reachable && strings.HasPrefix(candidate.Banner, "SSH-"):
		return "SSH confirmed"
	case candidate.Reachable:
		return "TCP open"
	default:
		return "unreachable"
	}
}

func candidateTitle(candidate probe.Candidate) string {
	name := candidate.Name
	if name == "" {
		name = "unnamed"
	}
	if candidate.Current {
		name += " [current]"
	}
	return fmt.Sprintf("%s  %s", name, candidate.InstanceID)
}

func candidateDescription(candidate probe.Candidate) string {
	return fmt.Sprintf("%s · %s · %s", candidateStatus(candidate), candidate.VpcID, strings.Join(candidate.VpcCIDRs, ", "))
}

type routeModel struct {
	routes   []config.NetworkRoute
	cursor   int
	start    int // first visible route index (scroll anchor)
	height   int // terminal height; 0 = unlimited (tests)
	width    int
	done     bool
	canceled bool
}

// routeItemLines is how many lines each route card renders: the CIDR line
// plus one metadata line below it.
const routeItemLines = 2

func (m routeModel) Init() tea.Cmd { return nil }

func routeItemHeights(routeCount int) []int {
	heights := make([]int, routeCount)
	for index := range heights {
		heights[index] = routeItemLines
	}
	return heights
}

func (m routeModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.height = message.Height
		m.width = message.Width
		return m, nil
	case tea.KeyMsg:
		switch message.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.routes)-1 {
				m.cursor++
			}
		case "pgup":
			m.cursor -= m.pageStep()
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "pgdown":
			m.cursor += m.pageStep()
			if m.cursor > len(m.routes)-1 {
				m.cursor = len(m.routes) - 1
			}
		case " ":
			m.routes[m.cursor].Selected = !m.routes[m.cursor].Selected
		case "a", "A":
			allSelected := true
			for _, route := range m.routes {
				if !route.Selected {
					allSelected = false
					break
				}
			}
			for index := range m.routes {
				m.routes[index].Selected = !allSelected
			}
		case "enter":
			m.done = true
			return m, tea.Quit
		case "esc", "q", "ctrl+c":
			m.canceled = true
			return m, tea.Quit
		default:
			heights := routeItemHeights(len(m.routes))
			m.start, _ = lineWindow(heights, m.cursor, m.start, listBudget(m.height))
			return m, nil
		}
	}
	heights := routeItemHeights(len(m.routes))
	m.start, _ = lineWindow(heights, m.cursor, m.start, listBudget(m.height))
	return m, nil
}

// pageStep is how far pgup/pgdn jump: the currently visible route count.
func (m routeModel) pageStep() int {
	budget := listBudget(m.height)
	if budget == math.MaxInt {
		return len(m.routes)
	}
	return max(budget/routeItemLines, 1)
}

func (m routeModel) chrome() Chrome {
	return Chrome{
		Title: "Select networks to tunnel",
		Info:  fmt.Sprintf("%d/%d · %d selected", m.cursor+1, len(m.routes), countSelectedRoutes(m.routes)),
		Hints: []Hint{
			{"↑/↓", "move"},
			{"pgup/pgdn", "page"},
			{"space", "toggle"},
			{"a", "all"},
			{"enter", "save"},
			{"q", "cancel"},
		},
	}
}

func countSelectedRoutes(routes []config.NetworkRoute) int {
	count := 0
	for _, route := range routes {
		if route.Selected {
			count++
		}
	}
	return count
}

func (m routeModel) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	heights := routeItemHeights(len(m.routes))
	if len(heights) > 0 {
		start, end := lineWindow(heights, m.cursor, m.start, m.chrome().Budget(m.height))
		b.WriteString(renderRoutes(m.routes, m.cursor, start, end))
		b.WriteString("\n")
	}
	return m.chrome().View(b.String(), width, m.height)
}

// renderRoutes draws the routes in [start,end). Line counts must stay
// consistent with routeItemHeights (cidr line + one metadata line).
func renderRoutes(routes []config.NetworkRoute, cursor, start, end int) string {
	var b strings.Builder
	for index := start; index < end; index++ {
		checked := "[ ]"
		if routes[index].Selected {
			checked = "[x]"
		}
		line := fmt.Sprintf("  %s %s", checked, routes[index].CIDR)
		if index == cursor {
			line = "> " + strings.TrimPrefix(line, "  ")
			line = selectedStyle.Render(line)
		}
		b.WriteString(line)
		metadata := routes[index].Source
		if routes[index].TargetID != "" {
			metadata += " · " + routes[index].TargetType + " " + routes[index].TargetID
		}
		if routes[index].Description != "" {
			metadata += " · " + routes[index].Description
		}
		fmt.Fprintf(&b, "\n      %s\n", descriptionStyle.Render(metadata))
	}
	trimmed := b.String()
	return strings.TrimSuffix(trimmed, "\n")
}

func SelectRoutes(routes []config.NetworkRoute) ([]config.NetworkRoute, bool, error) {
	if len(routes) == 0 {
		return routes, false, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return routes, false, nil
	}
	result, err := tea.NewProgram(routeModel{routes: append([]config.NetworkRoute(nil), routes...)}, tea.WithAltScreen()).Run()
	if err != nil {
		return nil, false, err
	}
	model := result.(routeModel)
	return model.routes, model.canceled, nil
}

func Warning(message string) string {
	return warningStyle.Render(message)
}

// Interactive reports whether both stdin and stdout are terminals.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// Pause waits for the user to press Enter so printed output can be read
// before the caller redraws (e.g. before the manage menu reopens). It is a
// no-op when stdin/stdout are not interactive.
func Pause(message string) {
	if !Interactive() {
		return
	}
	fmt.Printf("%s", message)
	_, _ = stdin.ReadString('\n')
}
