//go:build !race

// tcell's simulation screen reads its front buffer without the lock held while
// a draw is in flight, so polling GetContents from the test races the renderer
// under the race detector. The test is still exercised in normal runs.

package tea

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

type simulationModel struct {
	width  int
	height int
	status string
}

func (m simulationModel) Init() Cmd { return nil }

func (m simulationModel) Update(msg Msg) (Model, Cmd) {
	switch typed := msg.(type) {
	case WindowSizeMsg:
		m.width = typed.Width
		m.height = typed.Height
		m.status = "ready"
	case KeyMsg:
		if typed.String() == "q" {
			return m, Quit
		}
		m.status = "key:" + typed.String()
	}
	return m, nil
}

func (m simulationModel) View() string {
	return "size=" + strconv.Itoa(m.width) + "x" + strconv.Itoa(m.height) + " " + m.status + " [literal]"
}

func simulationContents(screen tcell.SimulationScreen) string {
	cells, width, height := screen.GetContents()
	rows := make([]string, 0, height)
	for row := 0; row < height; row++ {
		var builder strings.Builder
		for column := 0; column < width; column++ {
			cell := cells[row*width+column]
			if len(cell.Runes) == 0 {
				builder.WriteByte(' ')
				continue
			}
			builder.WriteString(string(cell.Runes))
		}
		rows = append(rows, strings.TrimRight(builder.String(), " "))
	}
	return strings.Join(rows, "\n")
}

func waitForContent(t *testing.T, screen tcell.SimulationScreen, needle string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(simulationContents(screen), needle) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; screen:\n%s", needle, simulationContents(screen))
}

func TestProgramRunsOnSimulationScreen(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")

	program := NewProgram(simulationModel{}, WithScreen(screen))
	result := make(chan Model, 1)
	runErr := make(chan error, 1)
	go func() {
		model, err := program.Run()
		if err != nil {
			runErr <- err
			return
		}
		result <- model
	}()

	<-program.ready
	waitForContent(t, screen, "ready [literal]")

	// Resize the simulated terminal and confirm the model sees it.
	screen.SetSize(60, 6)
	_ = screen.PostEvent(tcell.NewEventResize(60, 6))
	waitForContent(t, screen, "size=60x6 ready [literal]")

	// A key press goes through tview's input capture and reaches Update.
	screen.InjectKey(tcell.KeyRune, 'x', tcell.ModNone)
	waitForContent(t, screen, "key:x")

	// q returns tea.Quit and Run unwinds.
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-runErr:
		t.Fatalf("program returned error: %v", err)
	case model := <-result:
		final, ok := model.(simulationModel)
		if !ok {
			t.Fatalf("unexpected final model type %T", model)
		}
		if final.status != "key:x" {
			t.Fatalf("final status = %q, want key:x", final.status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("program did not quit after q")
	}
}
