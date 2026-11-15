// Package tea is a small Bubble Tea-compatible shim backed by tview/tcell.
//
// The bastionctl browser was written against the Bubble Tea runtime, but the
// terminal is now driven by tview. This package keeps the familiar
// Msg/Cmd/Model contract so the existing state machine, views, and tests stay
// untouched; only the runtime underneath changed.
package tea

import (
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Msg is any value delivered to a model's Update method.
type Msg = any

// Cmd is a deferred message producer. The runtime runs it in the background
// and feeds the returned message back into the update loop.
type Cmd func() Msg

// Model is the application state machine.
type Model interface {
	Init() Cmd
	Update(Msg) (Model, Cmd)
	View() string
}

// KeyType identifies a non-rune key press.
type KeyType int

const (
	KeyNull KeyType = iota
	KeyBreak
	KeyEnter
	KeyBackspace
	KeyTab
	KeyEsc
	KeyCtrlA
	KeyCtrlB
	KeyCtrlC
	KeyCtrlD
	KeyCtrlE
	KeyCtrlF
	KeyCtrlG
	KeyCtrlH
	KeyCtrlI
	KeyCtrlJ
	KeyCtrlK
	KeyCtrlL
	KeyCtrlM
	KeyCtrlN
	KeyCtrlO
	KeyCtrlP
	KeyCtrlQ
	KeyCtrlR
	KeyCtrlS
	KeyCtrlT
	KeyCtrlU
	KeyCtrlV
	KeyCtrlW
	KeyCtrlX
	KeyCtrlY
	KeyCtrlZ
	KeySpace
	KeyRunes
	KeyUp
	KeyDown
	KeyRight
	KeyLeft
	KeyShiftTab
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDown
	KeyDelete
	KeyInsert
)

// KeyEscape is an alias for KeyEsc.
const KeyEscape = KeyEsc

// KeyMsg reports one key press.
type KeyMsg struct {
	Type  KeyType
	Runes []rune
	Alt   bool
	Paste bool
}

// String renders the key the way Bubble Tea does ("enter", "down", "ctrl+c",
// or the literal runes).
func (k KeyMsg) String() string {
	switch k.Type {
	case KeyEnter:
		return "enter"
	case KeyBackspace:
		return "backspace"
	case KeyTab:
		return "tab"
	case KeyEsc:
		return "esc"
	case KeySpace:
		return " "
	case KeyRunes:
		if len(k.Runes) == 0 {
			return ""
		}
		return string(k.Runes)
	case KeyUp:
		return "up"
	case KeyDown:
		return "down"
	case KeyRight:
		return "right"
	case KeyLeft:
		return "left"
	case KeyShiftTab:
		return "shift+tab"
	case KeyHome:
		return "home"
	case KeyEnd:
		return "end"
	case KeyPgUp:
		return "pgup"
	case KeyPgDown:
		return "pgdown"
	case KeyDelete:
		return "delete"
	case KeyInsert:
		return "insert"
	case KeyBreak:
		return "break"
	case KeyNull:
		return ""
	}
	if name, ok := controlNames[k.Type]; ok {
		return name
	}
	return ""
}

var controlNames = map[KeyType]string{
	KeyCtrlA: "ctrl+a",
	KeyCtrlB: "ctrl+b",
	KeyCtrlC: "ctrl+c",
	KeyCtrlD: "ctrl+d",
	KeyCtrlE: "ctrl+e",
	KeyCtrlF: "ctrl+f",
	KeyCtrlG: "ctrl+g",
	KeyCtrlH: "ctrl+h",
	KeyCtrlI: "ctrl+i",
	KeyCtrlJ: "ctrl+j",
	KeyCtrlK: "ctrl+k",
	KeyCtrlL: "ctrl+l",
	KeyCtrlM: "ctrl+m",
	KeyCtrlN: "ctrl+n",
	KeyCtrlO: "ctrl+o",
	KeyCtrlP: "ctrl+p",
	KeyCtrlQ: "ctrl+q",
	KeyCtrlR: "ctrl+r",
	KeyCtrlS: "ctrl+s",
	KeyCtrlT: "ctrl+t",
	KeyCtrlU: "ctrl+u",
	KeyCtrlV: "ctrl+v",
	KeyCtrlW: "ctrl+w",
	KeyCtrlX: "ctrl+x",
	KeyCtrlY: "ctrl+y",
	KeyCtrlZ: "ctrl+z",
}

// WindowSizeMsg reports the terminal dimensions.
type WindowSizeMsg struct {
	Width  int
	Height int
}

// quitMsg terminates the runtime.
type quitMsg struct{}

// Quit is a command that stops the program.
func Quit() Msg { return quitMsg{} }

// batchMsg carries several commands to run concurrently.
type batchMsg struct {
	cmds []Cmd
}

// Batch runs the given commands concurrently. Nil commands are dropped; a
// single command is returned as-is, matching Bubble Tea.
func Batch(cmds ...Cmd) Cmd {
	runnable := make([]Cmd, 0, len(cmds))
	for _, cmd := range cmds {
		if cmd != nil {
			runnable = append(runnable, cmd)
		}
	}
	switch len(runnable) {
	case 0:
		return nil
	case 1:
		return runnable[0]
	}
	return func() Msg { return batchMsg{cmds: runnable} }
}

// Tick waits for the duration and then produces a message.
func Tick(duration time.Duration, fn func(time.Time) Msg) Cmd {
	return func() Msg {
		time.Sleep(duration)
		return fn(time.Now())
	}
}

// ProgramOption customizes a Program.
type ProgramOption func(*Program)

// WithAltScreen is accepted for API compatibility. tview/tcell always run on
// the alternate screen.
func WithAltScreen() ProgramOption {
	return func(*Program) {}
}

// WithScreen runs the program on a specific tcell screen instead of creating
// the default terminal screen. Tests use this with tcell's simulation screen.
func WithScreen(screen tcell.Screen) ProgramOption {
	return func(p *Program) { p.screen = screen }
}

// Program owns the tview application and the model update loop.
type Program struct {
	model  Model
	app    *tview.Application
	view   *tview.TextView
	screen tcell.Screen

	msgs  chan Msg
	sizes chan Msg
	done  chan struct{}
	ready chan struct{}

	appDone chan struct{}

	sizeMu     sync.Mutex
	lastWidth  int
	lastHeight int
}

// NewProgram builds a Program around the given model.
func NewProgram(model Model, options ...ProgramOption) *Program {
	program := &Program{
		model:   model,
		msgs:    make(chan Msg, 1024),
		sizes:   make(chan Msg, 1),
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
		appDone: make(chan struct{}),
	}
	for _, option := range options {
		option(program)
	}
	return program
}

// Send delivers a message to the running model. It is safe to call from any
// goroutine and becomes a no-op once the program has stopped.
func (p *Program) Send(msg Msg) {
	select {
	case p.msgs <- msg:
	case <-p.done:
	}
}

// Run starts the tview application and blocks until the model quits.
func (p *Program) Run() (Model, error) {
	p.app = tview.NewApplication()
	if p.screen != nil {
		p.app.SetScreen(p.screen)
	}
	p.view = tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false).
		SetScrollable(false)
	p.app.SetRoot(p.view, true)
	p.app.SetFocus(p.view)
	p.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		p.Send(keyMsgFromEvent(event))
		return nil
	})
	p.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		width, height := screen.Size()
		p.sizeMu.Lock()
		changed := width != p.lastWidth || height != p.lastHeight
		p.sizeMu.Unlock()
		if !changed {
			return false
		}
		select {
		case p.sizes <- WindowSizeMsg{Width: width, Height: height}:
			p.sizeMu.Lock()
			p.lastWidth, p.lastHeight = width, height
			p.sizeMu.Unlock()
		default:
			// The loop is busy; the next draw retries.
		}
		return false
	})

	runErr := make(chan error, 1)
	close(p.ready)
	go func() {
		defer close(p.appDone)
		runErr <- p.app.Run()
	}()

	p.loop()
	close(p.done)
	p.app.Stop()

	err := <-runErr
	if err != nil {
		return p.model, err
	}
	return p.model, nil
}

// loop drives Init and Update, rendering after every model change.
func (p *Program) loop() {
	p.runCmd(p.model.Init())
	for {
		select {
		case msg := <-p.msgs:
			if p.handle(msg) {
				return
			}
		case msg := <-p.sizes:
			if p.handle(msg) {
				return
			}
		case <-p.appDone:
			return
		}
	}
}

// handle applies one message. It reports whether the program should quit.
func (p *Program) handle(msg Msg) bool {
	if msg == nil {
		return false
	}
	if _, ok := msg.(quitMsg); ok {
		return true
	}
	model, cmd := p.model.Update(msg)
	p.model = model
	p.render()
	p.runCmd(cmd)
	return false
}

// runCmd executes a command off the update goroutine.
func (p *Program) runCmd(cmd Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		switch typed := msg.(type) {
		case nil:
			return
		case batchMsg:
			for _, nested := range typed.cmds {
				p.runCmd(nested)
			}
		default:
			p.Send(msg)
		}
	}()
}

// render pushes the current view into the tview text widget. The view text is
// ANSI, which tview translates into its own style tags; literal square
// brackets (e.g. "[current]") are escaped first so they survive.
func (p *Program) render() {
	select {
	case <-p.appDone:
		return
	default:
	}
	text := tview.TranslateANSI(tview.Escape(p.model.View()))
	rendered := make(chan struct{})
	go func() {
		p.app.QueueUpdateDraw(func() {
			p.view.SetText(text)
		})
		close(rendered)
	}()
	select {
	case <-rendered:
	case <-p.appDone:
	}
}

// keyMsgFromEvent maps a tcell key event to a Bubble Tea-style KeyMsg.
func keyMsgFromEvent(event *tcell.EventKey) KeyMsg {
	alt := event.Modifiers()&tcell.ModAlt != 0
	switch event.Key() {
	case tcell.KeyRune:
		return KeyMsg{Type: KeyRunes, Runes: []rune{event.Rune()}, Alt: alt}
	case tcell.KeyEnter:
		return KeyMsg{Type: KeyEnter, Alt: alt}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return KeyMsg{Type: KeyBackspace, Alt: alt}
	case tcell.KeyTab:
		return KeyMsg{Type: KeyTab, Alt: alt}
	case tcell.KeyBacktab:
		return KeyMsg{Type: KeyShiftTab, Alt: alt}
	case tcell.KeyEsc:
		return KeyMsg{Type: KeyEsc, Alt: alt}
	case tcell.KeyUp:
		return KeyMsg{Type: KeyUp, Alt: alt}
	case tcell.KeyDown:
		return KeyMsg{Type: KeyDown, Alt: alt}
	case tcell.KeyRight:
		return KeyMsg{Type: KeyRight, Alt: alt}
	case tcell.KeyLeft:
		return KeyMsg{Type: KeyLeft, Alt: alt}
	case tcell.KeyHome:
		return KeyMsg{Type: KeyHome, Alt: alt}
	case tcell.KeyEnd:
		return KeyMsg{Type: KeyEnd, Alt: alt}
	case tcell.KeyPgUp:
		return KeyMsg{Type: KeyPgUp, Alt: alt}
	case tcell.KeyPgDn:
		return KeyMsg{Type: KeyPgDown, Alt: alt}
	case tcell.KeyDelete:
		return KeyMsg{Type: KeyDelete, Alt: alt}
	case tcell.KeyInsert:
		return KeyMsg{Type: KeyInsert, Alt: alt}
	}
	if event.Key() >= tcell.KeyCtrlA && event.Key() <= tcell.KeyCtrlZ {
		return KeyMsg{Type: KeyCtrlA + KeyType(event.Key()-tcell.KeyCtrlA), Alt: alt}
	}
	return KeyMsg{Type: KeyNull, Alt: alt}
}
