package ui

// Demo generator (test-only): drives the real browser models headlessly —
// View() output is pure ANSI, no terminal needed — through a choreographed
// script with mock data, and writes an asciinema v2 .cast file.
//
// Run:
//
//	BASTIONCTL_DEMO_CAST=docs/demo.cast go test ./internal/ui -run TestGenerateDemoCast
//	agg --speed 1.6 docs/demo.cast docs/demo.gif
//
// The file lives in *_test.go and is excluded from the shipped binary.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"bastionctl/internal/config"
)

func init() {
	// Deterministic colors without a TTY: force the profile the cast painter
	// (agg) will use instead of lipgloss's stdin detection.
	lipgloss.SetColorProfile(termenv.ANSI256)
}

// demoOutputPath resolves the output path relative to the repo root (go test
// runs with the package directory as its working directory).
func demoOutputPath(output string) string {
	if filepath.IsAbs(output) {
		return output
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "..", "..", output)
}

func mockSummaries() []ConnSummary {
	return []ConnSummary{
		{
			Account: "123456789012", Name: "dns-split", Kind: "vpc", Method: "ssm",
			Region: "eu-central-1", Networks: "10.0.0.0/16, 172.31.0.0/16",
			DNS: "split (2 domain(s) via 172.31.0.2)", Running: true, PID: 2431,
		},
		{
			Account: "123456789012", Name: "corp", Kind: "vpc", Method: "ssh",
			Region: "eu-west-1", Networks: "172.16.0.0/12", DNS: "off",
		},
		{
			Account: "014498740021", Name: "legacy-app", Kind: "vpc", Method: "ssm",
			Region: "us-east-1", Networks: "10.20.0.0/16",
			DNS: "split (1 domain(s) via 10.20.0.2)", Running: true, PID: 8871, Stale: true,
		},
		{
			Account: "014498740021", Name: "ci-edge", Kind: "dedicated", Method: "ssh",
			Region: "us-west-2", Networks: "10.90.0.0/24", DNS: "off",
			Unreachable: true,
		},
	}
}

func mockDetailDNS() *ConnDetail {
	return &ConnDetail{
		Name: "dns-split", Kind: "vpc", Method: "ssm", Region: "eu-central-1", SSHPort: 22,
		Running: true, PID: 2431, Target: "i-00de18e4510b445fa", TargetOK: true,
		DNSSummary:   "split (2 domain(s) via 172.31.0.2)",
		DNSInstalled: "resolver files: /etc/resolver/example.internal",
		Domains:      []string{"example.internal", "svc.cluster.local"},
		Resolver:     []string{"172.31.0.2"},
		Routes: []config.NetworkRoute{
			{CIDR: "10.0.0.0/16", Selected: true, Source: "pivot_vpc"},
			{CIDR: "172.31.0.0/16", Selected: true, Source: "pivot_vpc"},
			{CIDR: "10.1.0.0/24", Selected: false, Source: "subnet_route53"},
		},
		Networks: []string{"10.0.0.0/16", "172.31.0.0/16"},
		Command:  "sshuttle -r i-00de18e4510b445fa 10.0.0.0/16 172.31.0.0/16 --ns-hosts 172.31.0.2",
	}
}

func mockDetailStale() *ConnDetail {
	return &ConnDetail{
		Name: "legacy-app", Kind: "vpc", Method: "ssm", Region: "us-east-1", SSHPort: 22,
		Running: true, PID: 8871, Target: "i-09a7d21b0c4e3f2a1", TargetOK: true,
		DNSSummary:      "split (1 domain(s) via 10.20.0.2)",
		DNSWantsInstall: true,
		Domains:         []string{"legacy.internal"},
		Resolver:        []string{"10.20.0.2"},
		Routes: []config.NetworkRoute{
			{CIDR: "10.20.0.0/16", Selected: true, Source: "pivot_vpc"},
		},
		Networks: []string{"10.20.0.0/16"},
		Command:  "sshuttle -r i-09a7d21b0c4e3f2a1 10.20.0.0/16 --ns-hosts 10.20.0.2",
	}
}

func mockDetailDown() *ConnDetail {
	return &ConnDetail{
		Name: "ci-edge", Kind: "dedicated", Method: "ssh", Region: "us-west-2", SSHPort: 22,
		Running: false, Target: "3.14.159.26", TargetOK: false,
		ReachDetail: "connection refused",
		Routes: []config.NetworkRoute{
			{CIDR: "10.90.0.0/24", Selected: true, Source: "manual", Description: "ci range"},
		},
		Networks: []string{"10.90.0.0/24"},
		Command:  "sshuttle -r 3.14.159.26 10.90.0.0/24",
	}
}

type castEvent struct {
	Elapsed float64 `json:"-"`
	Type    string  `json:"-"`
	Data    string  `json:"-"`
}

type demoDriver struct {
	model    appModel
	events   []castEvent
	elapsed  float64
	lastView string
}

func newDemoHome(width, height int) appModel {
	model := appModel{
		options: RunOptions{
			Account:   "123456789012 (prod) · 014498740021 (staging)",
			Summaries: mockSummaries(),
		},
		home: newHomeTable(mockSummaries(), 0),
	}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(appModel)
}

func newDemoDetail(width, height int, detail *ConnDetail) appModel {
	model := appModel{
		options: RunOptions{Level: 1, Detail: detail, Summaries: mockSummaries()},
		home:    newHomeTable(mockSummaries(), 0),
	}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(appModel)
}

func newDemoDriver(width, height int) *demoDriver {
	d := &demoDriver{model: newDemoHome(width, height)}
	d.capture(1.1)
	return d
}

// key feeds one keypress and records the resulting frame after the delay.
func (d *demoDriver) key(sequence string, delay float64) {
	updated, _ := d.model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(sequence)})
	d.model = updated.(appModel)
	d.capture(delay)
}

// typeSlowly types text character by character for the animation.
func (d *demoDriver) typeSlowly(text string, perKey float64) {
	for _, char := range []rune(text) {
		d.key(string(char), perKey)
	}
}

// open shows a connection's card, holding long enough to read it.
func (d *demoDriver) openCard(width, height int, detail *ConnDetail) {
	d.key("enter", 0.8)
	d.model = newDemoDetail(width, height, detail)
	d.capture(0.9)
}

// back returns to the home table after leaving a card.
func (d *demoDriver) back(width, height int, highlight int) {
	d.key("esc", 0.55)
	d.model = newDemoHome(width, height)
	// Keep the highlight on the moved-over row.
	summaries := mockSummaries()
	if highlight > 0 && highlight < len(summaries) {
		d.model.home.SetCursor(highlight)
	}
	d.capture(0.35)
}

// clearScreen resets the terminal position for one full-screen cast frame:
// without it, agg/painters keep the previous frame's pixels and content
// shifts by one row per write.
const castClearSequence = "\x1b[2J\x1b[H"

// capture records the current model view as one full-screen cast frame.
func (d *demoDriver) capture(delay float64) {
	view := castClearSequence + d.model.View()
	if view != d.lastView {
		d.events = append(d.events, castEvent{Elapsed: d.elapsed, Type: "o", Data: view})
		d.lastView = view
	}
	d.elapsed += delay
}

func writeCast(w io.Writer, width, height int, events []castEvent) error {
	duration := 0.0
	if len(events) > 0 {
		duration = events[len(events)-1].Elapsed
	}
	header, err := json.Marshal(map[string]any{
		"version":   2,
		"width":     width,
		"height":    height,
		"timestamp": time.Now().Unix(),
		"duration":  duration,
		"env":       map[string]string{"TERM": "xterm-256color", "SHELL": "/bin/zsh"},
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\n", header); err != nil {
		return err
	}
	for _, event := range events {
		data, err := json.Marshal(event.Data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "[%s, %q, %s]\n",
			strconv.FormatFloat(event.Elapsed, 'f', 3, 64), event.Type, data); err != nil {
			return err
		}
	}
	return nil
}

// TestGenerateDemoCast renders the mock demo script into an asciinema cast.
// Enabled by BASTIONCTL_DEMO_CAST=<path>; skipped in normal test runs.
func TestGenerateDemoCast(t *testing.T) {
	output := os.Getenv("BASTIONCTL_DEMO_CAST")
	if output == "" {
		t.Skip("BASTIONCTL_DEMO_CAST not set; skipping demo generation")
	}

	const width, height = 120, 26
	d := newDemoDriver(width, height)

	// 1. Walk the table down to the stale connection and back up.
	d.key("down", 0.4)
	d.key("down", 0.4)
	d.key("up", 0.35)
	d.key("up", 0.35)

	// 2. Open the healthy running connection.
	d.openCard(width, height, mockDetailDNS())
	d.key("esc", 0.3)
	d.back(width, height, 0)

	// 3. The unreachable one (red dot at the bottom).
	d.key("down", 0.35)
	d.key("down", 0.35)
	d.key("down", 0.35)
	d.openCard(width, height, mockDetailDown())
	d.key("esc", 0.3)
	d.back(width, height, 3)

	// 4. Command line: `stop corp`.
	d.key("up", 0.3)
	d.key("up", 0.3)
	d.key(":", 0.3)
	d.typeSlowly("stop corp", 0.09)
	d.capture(0.9)
	d.key("esc", 0.4)

	// 5. The stale one: card + `dns install` on the command line.
	d.key("down", 0.3)
	d.key("down", 0.3)
	d.openCard(width, height, mockDetailStale())
	d.key(":", 0.3)
	d.typeSlowly("dns install", 0.09)
	d.capture(0.9)

	// 6. Quit.
	d.key("q", 0.25)

	outputPath := demoOutputPath(output)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := writeCast(file, width, height, d.events); err != nil {
		t.Fatal(err)
	}
	t.Logf("cast written: %s (%d frames, %.1fs)", outputPath, len(d.events), d.elapsed)
}
