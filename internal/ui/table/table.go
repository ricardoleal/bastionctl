// Package table is a trimmed, Bubble Tea-compatible table widget backed by
// lipgloss. It mirrors the subset of charmbracelet/bubbles/table that
// bastionctl uses so the existing screens keep their exact rendering.
package table

import (
	"strings"

	tea "bastionctl/internal/ui/tea"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Row is one line of cells.
type Row []string

// Column defines a table column.
type Column struct {
	Title string
	Width int
}

// Styles carries the table's header, cell, and selection styles.
type Styles struct {
	Header   lipgloss.Style
	Cell     lipgloss.Style
	Selected lipgloss.Style
}

// DefaultStyles returns the Bubble Tea table defaults.
func DefaultStyles() Styles {
	return Styles{
		Selected: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")),
		Header:   lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:     lipgloss.NewStyle().Padding(0, 1),
	}
}

// Option configures a Model in New.
type Option func(*Model)

// Model is the table state.
type Model struct {
	cols   []Column
	rows   []Row
	cursor int
	focus  bool
	styles Styles

	width  int
	height int // visible data rows
	offset int // first visible data row
}

// New builds a table model.
func New(options ...Option) Model {
	model := Model{styles: DefaultStyles()}
	for _, option := range options {
		option(&model)
	}
	model.clamp()
	return model
}

// WithColumns sets the columns.
func WithColumns(cols []Column) Option {
	return func(m *Model) { m.cols = cols }
}

// WithRows sets the rows.
func WithRows(rows []Row) Option {
	return func(m *Model) { m.rows = rows }
}

// WithFocused sets the focus state.
func WithFocused(focused bool) Option {
	return func(m *Model) { m.focus = focused }
}

// WithHeight sets the total height, including the header line.
func WithHeight(height int) Option {
	return func(m *Model) { m.height = max(height-1, 0) }
}

// WithWidth sets the table width.
func WithWidth(width int) Option {
	return func(m *Model) { m.width = width }
}

// WithStyles sets the table styles.
func WithStyles(styles Styles) Option {
	return func(m *Model) { m.styles = styles }
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if !m.focus {
		return m, nil
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		m.moveUp(1)
	case "down", "j":
		m.moveDown(1)
	case "b", "pgup":
		m.moveUp(max(m.height, 1))
	case "f", "pgdown", " ":
		m.moveDown(max(m.height, 1))
	case "u", "ctrl+u":
		m.moveUp(max(m.height/2, 1))
	case "d", "ctrl+d":
		m.moveDown(max(m.height/2, 1))
	case "home", "g":
		m.moveUp(m.cursor)
	case "end", "G":
		m.moveDown(len(m.rows))
	}
	return m, nil
}

// View renders the header plus the visible rows.
func (m Model) View() string {
	var builder strings.Builder
	builder.WriteString(m.headersView())
	builder.WriteString("\n")
	if m.height <= 0 || len(m.rows) == 0 {
		return builder.String()
	}
	end := min(m.offset+m.height, len(m.rows))
	lines := make([]string, 0, m.height)
	for index := m.offset; index < end; index++ {
		lines = append(lines, m.renderRow(index))
	}
	builder.WriteString(strings.Join(lines, "\n"))
	return builder.String()
}

// Focused reports the focus state.
func (m Model) Focused() bool { return m.focus }

// Focus enables navigation.
func (m *Model) Focus() {
	m.focus = true
	m.clamp()
}

// Blur disables navigation.
func (m *Model) Blur() {
	m.focus = false
	m.clamp()
}

// Rows returns the current rows.
func (m Model) Rows() []Row { return m.rows }

// Columns returns the current columns.
func (m Model) Columns() []Column { return m.cols }

// SelectedRow returns the highlighted row, if any.
func (m Model) SelectedRow() Row {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}
	return m.rows[m.cursor]
}

// SetRows replaces the rows.
func (m *Model) SetRows(rows []Row) {
	m.rows = rows
	m.clamp()
}

// SetColumns replaces the columns.
func (m *Model) SetColumns(cols []Column) {
	m.cols = cols
	m.clamp()
}

// SetWidth sets the table width.
func (m *Model) SetWidth(width int) {
	m.width = width
	m.clamp()
}

// SetHeight sets the total height, including the header line.
func (m *Model) SetHeight(height int) {
	m.height = max(height-1, 0)
	m.clamp()
}

// Width returns the configured width.
func (m Model) Width() int { return m.width }

// Height returns the visible data row count.
func (m Model) Height() int { return m.height }

// Cursor returns the selected row index.
func (m Model) Cursor() int { return m.cursor }

// SetCursor selects a row.
func (m *Model) SetCursor(index int) {
	m.cursor = clamp(index, 0, len(m.rows)-1)
	m.clamp()
}

// MoveUp moves the selection up.
func (m *Model) MoveUp(n int) { m.moveUp(n) }

// MoveDown moves the selection down.
func (m *Model) MoveDown(n int) { m.moveDown(n) }

// GotoTop selects the first row.
func (m *Model) GotoTop() { m.moveUp(m.cursor) }

// GotoBottom selects the last row.
func (m *Model) GotoBottom() { m.moveDown(len(m.rows)) }

func (m *Model) moveUp(n int) {
	m.cursor = clamp(m.cursor-n, 0, len(m.rows)-1)
	m.clamp()
}

func (m *Model) moveDown(n int) {
	m.cursor = clamp(m.cursor+n, 0, len(m.rows)-1)
	m.clamp()
}

// clamp keeps the cursor and scroll offset inside the data set.
func (m *Model) clamp() {
	if len(m.rows) == 0 {
		m.cursor = 0
		m.offset = 0
		return
	}
	m.cursor = clamp(m.cursor, 0, len(m.rows)-1)
	if m.height <= 0 {
		m.offset = 0
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+m.height {
		m.offset = m.cursor - m.height + 1
	}
	m.offset = clamp(m.offset, 0, max(len(m.rows)-m.height, 0))
}

func (m Model) headersView() string {
	cells := make([]string, 0, len(m.cols))
	for _, column := range m.cols {
		if column.Width <= 0 {
			continue
		}
		style := lipgloss.NewStyle().Width(column.Width).MaxWidth(column.Width).Inline(true)
		rendered := style.Render(runewidth.Truncate(column.Title, column.Width, "…"))
		cells = append(cells, m.styles.Header.Render(rendered))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

func (m Model) renderRow(index int) string {
	cells := make([]string, 0, len(m.cols))
	for cellIndex, value := range m.rows[index] {
		if cellIndex >= len(m.cols) {
			break
		}
		column := m.cols[cellIndex]
		if column.Width <= 0 {
			continue
		}
		style := lipgloss.NewStyle().Width(column.Width).MaxWidth(column.Width).Inline(true)
		rendered := m.styles.Cell.Render(style.Render(runewidth.Truncate(value, column.Width, "…")))
		cells = append(cells, rendered)
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, cells...)
	if index == m.cursor {
		return m.styles.Selected.Render(row)
	}
	return row
}

func clamp(value, low, high int) int {
	if high < low {
		return high
	}
	return min(max(value, low), high)
}
