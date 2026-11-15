package tea

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestKeyMsgFromEvent(t *testing.T) {
	cases := []struct {
		name  string
		event *tcell.EventKey
		want  string
	}{
		{"rune", tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone), "q"},
		{"space", tcell.NewEventKey(tcell.KeyRune, ' ', tcell.ModNone), " "},
		{"enter", tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), "enter"},
		{"esc", tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone), "esc"},
		{"down", tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone), "down"},
		{"up", tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone), "up"},
		{"pgdown", tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone), "pgdown"},
		{"pgup", tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone), "pgup"},
		{"backspace", tcell.NewEventKey(tcell.KeyBackspace2, 0, tcell.ModNone), "backspace"},
		{"tab", tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone), "tab"},
		{"ctrl+c", tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone), "ctrl+c"},
		{"ctrl+u", tcell.NewEventKey(tcell.KeyCtrlU, 0, tcell.ModNone), "ctrl+u"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := keyMsgFromEvent(testCase.event).String()
			if got != testCase.want {
				t.Fatalf("String() = %q, want %q", got, testCase.want)
			}
		})
	}
}
