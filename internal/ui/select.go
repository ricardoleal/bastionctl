package ui

import (
	"fmt"
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
	selected int
	canceled bool
}

var (
	titleStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	selectedStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	descriptionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	warningStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

func (m selectModel) Init() tea.Cmd { return nil }

func (m selectModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.choices)-1 {
			m.cursor++
		}
	case "enter":
		m.selected = m.cursor
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.canceled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m selectModel) View() string {
	var output strings.Builder
	output.WriteString(titleStyle.Render(m.title))
	output.WriteString("\n\n")
	for index, choice := range m.choices {
		marker := "  "
		title := choice.Title
		if index == m.cursor {
			marker = "> "
			title = selectedStyle.Render(title)
		}
		fmt.Fprintf(&output, "%s%s", marker, title)
		if choice.Description != "" {
			fmt.Fprintf(&output, "\n    %s", descriptionStyle.Render(choice.Description))
		}
		output.WriteByte('\n')
	}
	output.WriteString("\n↑/↓ move  enter select  q cancel\n")
	return output.String()
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
	result, err := tea.NewProgram(initial).Run()
	if err != nil {
		return 0, false, err
	}
	model := result.(selectModel)
	return model.selected, model.canceled, nil
}

type ConnectionChoice struct {
	Status   string
	Name     string
	Kind     string
	Region   string
	Networks string
}

func SelectConnection(choices []ConnectionChoice) (int, bool, error) {
	if len(choices) == 0 {
		return 0, true, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Println("Configured connections")
		for index, choice := range choices {
			fmt.Printf("  %d. %-15s %-24s %-10s %-16s %s\n", index+1, choice.Status, choice.Name, choice.Kind, choice.Region, choice.Networks)
		}
		return PromptSelect(len(choices), 1) - 1, false, nil
	}

	model := newConnectionModel(choices)
	result, err := tea.NewProgram(model).Run()
	if err != nil {
		return 0, false, err
	}
	selected := result.(connectionModel)
	return selected.selected, selected.canceled, nil
}

type connectionModel struct {
	choices  []ConnectionChoice
	table    table.Model
	selected int
	canceled bool
}

func newConnectionModel(choices []ConnectionChoice) connectionModel {
	tableModel := table.New(
		table.WithColumns(connectionColumns(120)),
		table.WithRows(connectionRows(choices, 120)),
		table.WithFocused(true),
		table.WithHeight(10),
		table.WithStyles(sharedTableStyles()),
	)
	return connectionModel{choices: choices, table: tableModel, selected: -1}
}

func (m connectionModel) Init() tea.Cmd { return nil }

func (m connectionModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.table.SetWidth(max(message.Width, 40))
		m.table.SetHeight(max(message.Height-7, 4))
		safeSetColumnsRows(&m.table, connectionColumns(message.Width), connectionRows(m.choices, message.Width))
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

func (m connectionModel) View() string {
	header := fmt.Sprintf("%s  %s", titleStyle.Render("Configured connections"), descriptionStyle.Render(fmt.Sprintf("%d entries", len(m.choices))))
	position := descriptionStyle.Render(fmt.Sprintf("%d/%d", m.table.Cursor()+1, len(m.choices)))
	return header + "\n\n" + m.table.View() + "\n" + position + "  ↑/↓ move  pgup/pgdn jump  enter select  q cancel\n"
}

func connectionColumns(width int) []table.Column {
	switch {
	case width >= 110:
		return []table.Column{
			{Title: "Status", Width: 15},
			{Title: "Connection", Width: 28},
			{Title: "Kind", Width: 10},
			{Title: "Region", Width: 16},
			{Title: "Networks", Width: max(width-77, 20)},
		}
	case width >= 80:
		return []table.Column{
			{Title: "Status", Width: 15},
			{Title: "Connection", Width: 24},
			{Title: "Region", Width: 16},
			{Title: "Networks", Width: max(width-63, 14)},
		}
	default:
		return []table.Column{
			{Title: "Status", Width: 15},
			{Title: "Connection", Width: max(width-37, 14)},
			{Title: "Region", Width: 16},
		}
	}
}

func connectionRows(choices []ConnectionChoice, width int) []table.Row {
	rows := make([]table.Row, len(choices))
	for index, choice := range choices {
		switch {
		case width >= 110:
			rows[index] = table.Row{choice.Status, choice.Name, choice.Kind, choice.Region, choice.Networks}
		case width >= 80:
			rows[index] = table.Row{choice.Status, choice.Name, choice.Region, choice.Networks}
		default:
			rows[index] = table.Row{choice.Status, choice.Name, choice.Region}
		}
	}
	return rows
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

func SelectCandidate(candidates []probe.Candidate) (int, bool, error) {
	if len(candidates) == 0 {
		return 0, true, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		choices := make([]Choice, len(candidates))
		for index, candidate := range candidates {
			choices[index] = Choice{
				Title:       candidateTitle(candidate),
				Description: candidateDescription(candidate),
			}
		}
		return Select("Select a pivot instance", choices)
	}

	model := newCandidateModel(candidates)
	result, err := tea.NewProgram(model).Run()
	if err != nil {
		return 0, false, err
	}
	selected := result.(candidateModel)
	return selected.selected, selected.canceled, nil
}

type candidateModel struct {
	candidates []probe.Candidate
	table      table.Model
	width      int
	selected   int
	canceled   bool
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
		m.table.SetHeight(max(message.Height-7, 4))
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
	header := fmt.Sprintf("%s  %s", titleStyle.Render("Select a pivot instance"), descriptionStyle.Render(fmt.Sprintf("%d candidates", len(m.candidates))))
	position := descriptionStyle.Render(fmt.Sprintf("%d/%d", m.table.Cursor()+1, len(m.candidates)))
	return header + "\n\n" + m.table.View() + "\n" + position + "  ↑/↓ move  pgup/pgdn jump  enter select  q cancel\n"
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
	return fmt.Sprintf("%s  %s", name, candidate.InstanceID)
}

func candidateDescription(candidate probe.Candidate) string {
	return fmt.Sprintf("%s · %s · %s", candidateStatus(candidate), candidate.VpcID, strings.Join(candidate.VpcCIDRs, ", "))
}

type routeModel struct {
	routes   []config.NetworkRoute
	cursor   int
	done     bool
	canceled bool
}

func (m routeModel) Init() tea.Cmd { return nil }

func (m routeModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.routes)-1 {
			m.cursor++
		}
	case " ":
		m.routes[m.cursor].Selected = !m.routes[m.cursor].Selected
	case "enter":
		m.done = true
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.canceled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m routeModel) View() string {
	var output strings.Builder
	output.WriteString(titleStyle.Render("Select networks to tunnel"))
	output.WriteString("\n\n")
	for index, route := range m.routes {
		cursor := "  "
		if index == m.cursor {
			cursor = "> "
		}
		checked := "[ ]"
		if route.Selected {
			checked = "[x]"
		}
		line := fmt.Sprintf("%s%s %s", cursor, checked, route.CIDR)
		if index == m.cursor {
			line = selectedStyle.Render(line)
		}
		output.WriteString(line)
		metadata := route.Source
		if route.TargetID != "" {
			metadata += " · " + route.TargetType + " " + route.TargetID
		}
		if route.Description != "" {
			metadata += " · " + route.Description
		}
		fmt.Fprintf(&output, "\n      %s\n", descriptionStyle.Render(metadata))
	}
	output.WriteString("\n↑/↓ move  space toggle  enter save  q cancel\n")
	return output.String()
}

func SelectRoutes(routes []config.NetworkRoute) ([]config.NetworkRoute, bool, error) {
	if len(routes) == 0 {
		return routes, false, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return routes, false, nil
	}
	result, err := tea.NewProgram(routeModel{routes: append([]config.NetworkRoute(nil), routes...)}).Run()
	if err != nil {
		return nil, false, err
	}
	model := result.(routeModel)
	return model.routes, model.canceled, nil
}

func Warning(message string) string {
	return warningStyle.Render(message)
}
