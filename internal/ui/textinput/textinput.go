// Package textinput is a minimal Bubble Tea-compatible text input backed by
// plain runes. It covers the subset bastionctl needs (single-line entry with
// optional password masking).
package textinput

import (
	"strings"

	tea "bastionctl/internal/ui/tea"
)

// EchoMode controls how the value is displayed.
type EchoMode int

const (
	// EchoNormal shows the typed characters.
	EchoNormal EchoMode = iota
	// EchoPassword masks the value with EchoCharacter.
	EchoPassword
	// EchoNone hides the value entirely.
	EchoNone
)

// Model is the input state.
type Model struct {
	EchoMode      EchoMode
	EchoCharacter rune
	CharLimit     int

	value   []rune
	pos     int
	focused bool
}

// New returns an empty input model.
func New() Model {
	return Model{EchoCharacter: '•'}
}

// Blink is accepted for API compatibility; tview owns cursor rendering.
var Blink tea.Cmd = func() tea.Msg { return nil }

// Init implements tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// Focus marks the input active and returns the (no-op) blink command.
func (m *Model) Focus() tea.Cmd {
	m.focused = true
	return Blink
}

// Blur marks the input inactive.
func (m *Model) Blur() {
	m.focused = false
}

// Focused reports the focus state.
func (m Model) Focused() bool { return m.focused }

// Value returns the typed text.
func (m Model) Value() string { return string(m.value) }

// SetValue replaces the typed text.
func (m *Model) SetValue(value string) {
	m.value = []rune(value)
	m.pos = len(m.value)
}

// Reset clears the input.
func (m *Model) Reset() {
	m.value = nil
	m.pos = 0
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.Type {
	case tea.KeyRunes:
		m.insert(string(key.Runes))
	case tea.KeySpace:
		m.insert(" ")
	case tea.KeyBackspace:
		m.deleteBackward()
	case tea.KeyDelete:
		m.deleteForward()
	case tea.KeyLeft, tea.KeyCtrlB:
		if m.pos > 0 {
			m.pos--
		}
	case tea.KeyRight, tea.KeyCtrlF:
		if m.pos < len(m.value) {
			m.pos++
		}
	case tea.KeyHome, tea.KeyCtrlA:
		m.pos = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		m.pos = len(m.value)
	case tea.KeyCtrlU:
		m.value = m.value[m.pos:]
		m.pos = 0
	case tea.KeyCtrlK:
		m.value = m.value[:m.pos]
	}
	return m, nil
}

// View renders the (optionally masked) value with a cursor while focused.
func (m Model) View() string {
	if m.EchoMode == EchoNone {
		return ""
	}
	text := string(m.value)
	if m.EchoMode == EchoPassword {
		character := m.EchoCharacter
		if character == 0 {
			character = '•'
		}
		text = strings.Repeat(string(character), len(m.value))
	}
	if m.focused {
		return text + "|"
	}
	return text
}

func (m *Model) insert(text string) {
	if text == "" {
		return
	}
	runes := []rune(text)
	if m.CharLimit > 0 && len(m.value)+len(runes) > m.CharLimit {
		room := m.CharLimit - len(m.value)
		if room <= 0 {
			return
		}
		runes = runes[:room]
	}
	updated := make([]rune, 0, len(m.value)+len(runes))
	updated = append(updated, m.value[:m.pos]...)
	updated = append(updated, runes...)
	updated = append(updated, m.value[m.pos:]...)
	m.value = updated
	m.pos += len(runes)
}

func (m *Model) deleteBackward() {
	if m.pos <= 0 {
		return
	}
	m.value = append(m.value[:m.pos-1], m.value[m.pos:]...)
	m.pos--
}

func (m *Model) deleteForward() {
	if m.pos >= len(m.value) {
		return
	}
	m.value = append(m.value[:m.pos], m.value[m.pos+1:]...)
}
