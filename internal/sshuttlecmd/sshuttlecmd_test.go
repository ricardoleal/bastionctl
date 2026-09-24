package sshuttlecmd

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"bastionctl/internal/config"
)

func TestPrintCommandDoesNotElevateSSHClient(t *testing.T) {
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = originalStdout })

	PrintCommand("/opt/homebrew/bin/sshuttle", []string{"-r", "i-0123456789abcdef0", "10.100.0.0/16"})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	command := "  /opt/homebrew/bin/sshuttle -r i-0123456789abcdef0 10.100.0.0/16"
	if !strings.Contains(string(output), command) {
		t.Fatalf("output does not contain %q: %q", command, output)
	}
	if strings.Contains(string(output), "sudo /opt/homebrew/bin/sshuttle") {
		t.Fatalf("command elevates sshuttle and loses the user's SSH config: %q", output)
	}
}

func TestBuildDaemonArgs(t *testing.T) {
	args := []string{"-r", "i-0123456789abcdef0", "10.100.0.0/16"}
	got := buildDaemonArgs(args, "/tmp/bastionctl/sshuttle.pid")
	want := []string{
		"--daemon", "--verbose", "--pidfile", "/tmp/bastionctl/sshuttle.pid",
		"-r", "i-0123456789abcdef0", "10.100.0.0/16",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildDaemonArgs() = %q, want %q", got, want)
	}
}

func TestLogCommands(t *testing.T) {
	commands, err := logCommands("darwin")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[1][1] != "stream" {
		t.Fatalf("darwin log commands = %#v", commands)
	}
	linuxCommand, err := linuxLogCommand(
		func(string) (string, error) { return "/usr/bin/journalctl", nil },
		func(string) bool { return false },
	)
	if err != nil {
		t.Fatal(err)
	}
	if linuxCommand[0] != "journalctl" {
		t.Fatalf("linux log command = %#v", linuxCommand)
	}
}

func TestLinuxLogCommandFallsBackToSyslog(t *testing.T) {
	command, err := linuxLogCommand(
		func(string) (string, error) { return "", exec.ErrNotFound },
		func(path string) bool { return path == "/var/log/syslog" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if command[len(command)-1] != "/var/log/syslog" {
		t.Fatalf("fallback command = %#v", command)
	}
}

func TestBuildArgsUsesSelectedRoutes(t *testing.T) {
	cfg := &config.Config{
		Target:   "i-0123456789abcdef0",
		VpcCIDRs: []string{"10.0.0.0/16"},
		Routes: []config.NetworkRoute{
			{CIDR: "10.0.0.0/16", Selected: false},
			{CIDR: "10.20.0.0/16", Selected: true},
		},
	}
	want := []string{"-r", "i-0123456789abcdef0", "10.20.0.0/16"}
	if got := BuildArgs(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgs() = %q, want %q", got, want)
	}
}

func TestWaitForPIDFileHandlesDelayedWrite(t *testing.T) {
	pidFile := t.TempDir() + "/sshuttle.pid"
	if err := os.WriteFile(pidFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(pidFile, []byte("16645\n"), 0o600)
	}()

	pid, err := waitForPIDFile(pidFile, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 16645 {
		t.Fatalf("pid = %s, want 16645", strconv.Itoa(pid))
	}
}

func TestStopBackgroundTerminatesRecordedSSHuttleProcess(t *testing.T) {
	dir := t.TempDir()
	sshuttlePath := filepath.Join(dir, "sshuttle")
	if err := os.Symlink("/bin/sleep", sshuttlePath); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(sshuttlePath, "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	pidFile := filepath.Join(dir, "connection.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(command.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	statusPID, running, err := BackgroundStatus(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if statusPID != command.Process.Pid || !running {
		t.Fatalf("BackgroundStatus() = %d, %v", statusPID, running)
	}

	pid, wasRunning, err := StopBackground(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if pid != command.Process.Pid || !wasRunning {
		t.Fatalf("StopBackground() = %d, %v", pid, wasRunning)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("PID file still exists: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sshuttle test process did not exit")
	}
}

func TestIsSSHuttleCommandRejectsSimilarName(t *testing.T) {
	if isSSHuttleCommand("/tmp/sshuttlecmd.test -test.run Test") {
		t.Fatal("accepted a similarly named non-sshuttle process")
	}
	if !isSSHuttleCommand("/usr/bin/python3 /usr/local/bin/sshuttle --daemon") {
		t.Fatal("rejected a Python-launched sshuttle process")
	}
}
