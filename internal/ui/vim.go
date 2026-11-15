package ui

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"bastionctl/internal/config"
	tea "bastionctl/internal/ui/tea"
)

type vimSaveFinishedMsg struct {
	editor       *connectionEditor
	cfg          *config.Config
	originalName string
	wasNew       bool
	closeEditor  bool
	err          error
}

func (m appModel) updateVimPrompt(key string, runes []rune) (tea.Model, tea.Cmd) {
	editor := m.editor
	switch key {
	case "esc":
		editor.vimPrompt, editor.vimInput = "", ""
	case "backspace":
		input := []rune(editor.vimInput)
		if len(input) > 0 {
			editor.vimInput = string(input[:len(input)-1])
		}
	case "enter":
		prompt, input := editor.vimPrompt, editor.vimInput
		editor.vimPrompt, editor.vimInput = "", ""
		if prompt == "/" {
			editor.searchYAML(input)
			return m, nil
		}
		switch strings.TrimSpace(input) {
		case "w":
			return m.saveVimEditor(false)
		case "wq", "x":
			return m.saveVimEditor(true)
		case "q":
			if editor.isDirty() {
				m.setError("unsaved changes; use :w or :q!")
				return m, nil
			}
			m.closeVimEditor()
		case "q!":
			m.closeVimEditor()
		default:
			m.setError("unknown editor command: " + input)
		}
	default:
		if len(runes) > 0 {
			editor.vimInput += string(runes)
		}
	}
	return m, nil
}

func (editor *connectionEditor) isDirty() bool {
	if editor.yamlMode {
		return strings.Join(editor.yamlLines, "\n") != editor.yamlBaseline
	}
	return editor.isNew || !reflect.DeepEqual(editor.values, editor.savedValues) || !reflect.DeepEqual(editor.routes, editor.savedRoutes)
}

func (m *appModel) closeVimEditor() {
	if m.editor.yamlMode {
		m.editor.yamlMode = false
		m.editor.yamlLines = nil
		m.editor.vimInsert = false
		m.editor.vimUndo = nil
		return
	}
	m.editor = nil
}

func (editor *connectionEditor) searchYAML(query string) {
	if query == "" {
		return
	}
	for offset := 0; offset < len(editor.yamlLines); offset++ {
		row := (editor.yamlRow + offset) % len(editor.yamlLines)
		line := editor.yamlLines[row]
		start := 0
		if offset == 0 {
			start = min(editor.yamlCol+1, utf8.RuneCountInString(line))
		}
		tail := string([]rune(line)[start:])
		lowerTail := strings.ToLower(tail)
		if index := strings.Index(lowerTail, strings.ToLower(query)); index >= 0 {
			editor.yamlRow = row
			editor.yamlCol = start + utf8.RuneCountInString(lowerTail[:index])
			return
		}
	}
	lowerLine := strings.ToLower(editor.yamlLines[editor.yamlRow])
	if index := strings.Index(lowerLine, strings.ToLower(query)); index >= 0 {
		editor.yamlCol = utf8.RuneCountInString(lowerLine[:index])
	}
}

func (m appModel) saveVimEditor(closeEditor bool) (tea.Model, tea.Cmd) {
	editor := m.editor
	if editor.conflicted {
		m.setError("connection changed on disk; reopen before saving")
		return m, nil
	}
	if editor.isNew && m.options.SaveNewConfig == nil || !editor.isNew && m.options.SaveConfig == nil {
		m.setError("edit: saving is unavailable")
		return m, nil
	}
	draft := *editor
	if editor.yamlMode {
		if err := applyYAMLToEditor(&draft); err != nil {
			m.setError("yaml: " + err.Error())
			return m, nil
		}
	}
	cfg, err := editorConfig(&draft)
	if err != nil {
		m.setError("edit: " + err.Error())
		return m, nil
	}
	name, wasNew := editor.originalName, editor.isNew
	saver := m.options.SaveConfig
	if wasNew {
		name = cfg.Name
		saver = func(_ string, value *config.Config) error { return m.options.SaveNewConfig(value) }
	}
	editor.vimSaving = true
	return m, func() (message tea.Msg) {
		result := vimSaveFinishedMsg{editor: editor, cfg: cfg, originalName: name, wasNew: wasNew, closeEditor: closeEditor}
		defer func() {
			if recovered := recover(); recovered != nil {
				result.err = fmt.Errorf("saving connection: %v", recovered)
			}
			message = result
		}()
		result.err = saver(name, cfg)
		return result
	}
}

func (m appModel) finishVimSave(message vimSaveFinishedMsg) (tea.Model, tea.Cmd) {
	if m.editor != message.editor {
		return m, nil
	}
	editor := m.editor
	editor.vimSaving = false
	if message.err != nil {
		m.setError("edit: " + message.err.Error())
		return m, nil
	}
	if editor.conflicted {
		for _, known := range m.knownConfigs {
			if known.Name == message.cfg.Name && known.AccountID == message.cfg.AccountID && reflect.DeepEqual(known, message.cfg) {
				editor.conflicted = false
				if strings.Contains(m.errorMsg, "connection changed on disk") {
					m.clearError()
				}
				break
			}
		}
	}
	if message.wasNew {
		found := false
		for _, summary := range m.options.Summaries {
			if summary.Name == message.cfg.Name && summary.Account == message.cfg.AccountID {
				found = true
				break
			}
		}
		if !found {
			summary := summaryFromConfig(message.cfg, ConnSummary{})
			summary.Reachable = true
			m.options.Summaries = append(m.options.Summaries, summary)
		}
	}
	m.applySavedConfig(message.originalName, message.cfg, false, false)
	if editor.yamlMode {
		rebuilt := newConnectionEditor(message.cfg, editor.profiles, editor.regions)
		editor.values, editor.routes, editor.cfg = rebuilt.values, rebuilt.routes, message.cfg
		editor.field = min(editor.field, len(editor.values)-1)
		editor.input = editor.values[editor.field]
		editor.yamlBaseline = strings.Join(editor.yamlLines, "\n")
	} else {
		editor.cfg = message.cfg
	}
	editor.savedValues = append([]string(nil), editor.values...)
	editor.savedRoutes = append([]config.NetworkRoute(nil), editor.routes...)
	editor.originalName = message.cfg.Name
	editor.isNew = false
	editor.vimUndo = nil
	if message.closeEditor {
		m.closeVimEditor()
	}
	if m.options.LoadDetail != nil && (message.wasNew || m.options.Level == 1 && m.options.Detail != nil && m.options.Detail.Name == message.originalName) {
		name, loader := message.cfg.Name, m.options.LoadDetail
		m.loadingDetail = name
		return m, func() tea.Msg {
			detail, err := loader(name)
			return detailLoadedMsg{name: name, detail: detail, err: err}
		}
	}
	return m, nil
}

type editorSnapshot struct {
	values        []string
	lines         []string
	field, cursor int
	row, col      int
}

func renderVimLine(line string, cursor int, insertMode, yamlMode bool, width int) string {
	var output strings.Builder
	position := 0
	writeToken := func(text string, style lipgloss.Style) {
		characters := []rune(text)
		if cursor >= position && cursor < position+len(characters) {
			index := cursor - position
			output.WriteString(style.Render(string(characters[:index])))
			cursorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11"))
			if insertMode {
				cursorStyle = cursorStyle.Background(lipgloss.Color("14"))
			}
			output.WriteString(cursorStyle.Render(string(characters[index])))
			output.WriteString(style.Render(string(characters[index+1:])))
		} else {
			output.WriteString(style.Render(text))
		}
		position += len(characters)
	}
	if yamlMode {
		lexer := lexers.Get("yaml")
		iterator, err := lexer.Tokenise(nil, line)
		if err == nil {
			for token := iterator(); token != chroma.EOF; token = iterator() {
				text := strings.TrimSuffix(token.Value, "\n")
				style := lipgloss.NewStyle()
				switch {
				case token.Type.InCategory(chroma.Comment):
					style = chromeDimStyle
				case token.Type.InCategory(chroma.Name):
					style = chromeCellStyle
				case token.Type.InCategory(chroma.Keyword), token.Type.InSubCategory(chroma.LiteralNumber):
					style = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
				case token.Type.InSubCategory(chroma.LiteralString):
					style = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
				}
				writeToken(text, style)
			}
		} else {
			writeToken(line, lipgloss.NewStyle())
		}
	} else {
		writeToken(line, lipgloss.NewStyle())
	}
	if cursor >= 0 && cursor >= position {
		style := lipgloss.NewStyle().Background(lipgloss.Color("11"))
		if insertMode {
			style = style.Background(lipgloss.Color("14"))
		}
		output.WriteString(style.Render(" "))
	}
	if width <= 0 {
		return output.String()
	}
	start := 0
	if cursor >= 0 {
		characters := []rune(line)
		cursorWidth := lipgloss.Width(string(characters[:min(cursor, len(characters))]))
		characterWidth := 1
		if cursor < len(characters) {
			characterWidth = max(lipgloss.Width(string(characters[cursor])), 1)
		}
		target := max(cursorWidth-width+characterWidth, 0)
		for index := 0; index <= len(characters); index++ {
			start = lipgloss.Width(string(characters[:index]))
			if start >= target {
				break
			}
		}
	}
	return ansi.Cut(output.String(), start, start+width)
}

func (editor *connectionEditor) checkpoint() {
	snapshot := editorSnapshot{
		values: append([]string(nil), editor.values...),
		lines:  append([]string(nil), editor.yamlLines...),
		field:  editor.field, cursor: editor.cursor,
		row: editor.yamlRow, col: editor.yamlCol,
	}
	editor.vimUndo = append(editor.vimUndo, snapshot)
	if len(editor.vimUndo) > 100 {
		editor.vimUndo = editor.vimUndo[1:]
	}
}

func (editor *connectionEditor) undoEdit() {
	if len(editor.vimUndo) == 0 {
		return
	}
	snapshot := editor.vimUndo[len(editor.vimUndo)-1]
	editor.vimUndo = editor.vimUndo[:len(editor.vimUndo)-1]
	editor.values, editor.yamlLines = snapshot.values, snapshot.lines
	editor.field, editor.cursor = snapshot.field, snapshot.cursor
	editor.yamlRow, editor.yamlCol = snapshot.row, snapshot.col
	editor.input = editor.values[editor.field]
}

func (editor *connectionEditor) vimKey(key string, runes []rune) (string, bool) {
	if editor.vimInsert {
		if key == "esc" || key == "ctrl+o" {
			editor.vimInsert = false
			if editor.yamlMode && editor.yamlCol > 0 {
				editor.yamlCol--
			} else if !editor.yamlMode && editor.cursor > 0 {
				editor.cursor--
			}
			return key, true
		}
		if !editor.yamlMode && (key == "enter" || key == "tab") {
			editor.vimInsert = false
			return key, false
		}
		if key == "backspace" || key == "delete" || editor.yamlMode && (key == "enter" || key == "tab") || len(runes) > 0 {
			editor.checkpoint()
		}
		if len(runes) > 0 && !editor.yamlMode {
			text := string(runes)
			editor.input = insertAtRune(editor.input, editor.cursor, text)
			editor.cursor += utf8.RuneCountInString(text)
			editor.values[editor.field] = editor.input
			return key, true
		}
		return key, false
	}
	if editor.vimPending {
		editor.vimPending = false
		if key == "d" && editor.yamlMode {
			editor.checkpoint()
			if len(editor.yamlLines) == 1 {
				editor.yamlLines[0] = ""
			} else {
				editor.yamlLines = append(editor.yamlLines[:editor.yamlRow], editor.yamlLines[editor.yamlRow+1:]...)
			}
			editor.yamlClampCol()
			return key, true
		}
	}
	switch key {
	case "i", "a":
		if !editor.yamlMode && (len(editorFieldChoices(editor)) > 0 || editor.fields[editor.field] == "routes") {
			return key, true
		}
		if key == "a" {
			if editor.yamlMode {
				editor.yamlCol = min(editor.yamlCol+1, utf8.RuneCountInString(editor.yamlLines[editor.yamlRow]))
			} else {
				editor.cursor = min(editor.cursor+1, utf8.RuneCountInString(editor.input))
			}
		}
		editor.vimInsert = true
		return key, true
	case "h":
		return "left", false
	case "j":
		return "down", false
	case "k":
		return "up", false
	case "l":
		return "right", false
	case "x":
		if !editor.yamlMode && (len(editorFieldChoices(editor)) > 0 || editor.fields[editor.field] == "routes") {
			return key, true
		}
		if editor.yamlMode && editor.yamlCol >= utf8.RuneCountInString(editor.yamlLines[editor.yamlRow]) {
			return key, true
		}
		editor.checkpoint()
		return "delete", false
	case "u":
		editor.undoEdit()
		return key, true
	case "d":
		if editor.yamlMode {
			editor.vimPending = true
		}
		return key, true
	case "o":
		if editor.yamlMode {
			editor.checkpoint()
			index := editor.yamlRow + 1
			editor.yamlLines = append(editor.yamlLines, "")
			copy(editor.yamlLines[index+1:], editor.yamlLines[index:])
			editor.yamlLines[index] = ""
			editor.yamlRow, editor.yamlCol, editor.vimInsert = index, 0, true
		}
		return key, true
	case "esc":
		return key, true
	case "enter", "tab":
		return key, editor.yamlMode
	case "v":
		return key, editor.yamlMode
	case "up", "down", "left", "right", "home", "end", "pgup", "pgdown", "ctrl+y", "ctrl+s":
		return key, false
	default:
		return key, true
	}
}
