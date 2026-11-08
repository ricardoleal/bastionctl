package sshuttlecmd

import (
	"context"
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

func TestStartBackgroundCapturedReturnsChildOutput(t *testing.T) {
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "sshuttle.pid")
	script := filepath.Join(directory, "fake-sshuttle")
	contents := "#!/bin/sh\n" +
		"printf 'captured stdout\\n'\n" +
		"printf 'captured stderr\\n' >&2\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = '--pidfile' ]; then shift; printf '12345\\n' > \"$1\"; break; fi\n" +
		"  shift\n" +
		"done\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	pid, output, err := StartBackgroundCaptured(context.Background(), script, []string{"-r", "i-1"}, pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 12345 || !strings.Contains(output, "captured stdout") || !strings.Contains(output, "captured stderr") {
		t.Fatalf("pid=%d output=%q", pid, output)
	}
}

func TestStartCommandStreamCapturesCombinedOutput(t *testing.T) {
	session := StartCommandStream(context.Background(), "/bin/sh", []string{"-c", "printf 'launch output\\n'; printf 'launch error\\n' >&2"}, nil)
	var output []string
	for line := range session.Lines {
		output = append(output, line)
	}
	if err := <-session.Done; err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(output, "\n")
	if !strings.Contains(joined, "launch output") || !strings.Contains(joined, "launch error") {
		t.Fatalf("captured output = %q", joined)
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

func TestDNSArgsByMode(t *testing.T) {
	if got := DNSArgs(nil); got != nil {
		t.Fatalf("DNSArgs(nil) = %q", got)
	}
	if got := DNSArgs(&config.DNSConfig{}); got != nil {
		t.Fatalf("DNSArgs(off) = %q", got)
	}
	if got := DNSArgs(&config.DNSConfig{Mode: config.DNSModeAll}); !reflect.DeepEqual(got, []string{"--dns"}) {
		t.Fatalf("DNSArgs(all) = %q", got)
	}
	if got := DNSArgs(&config.DNSConfig{Mode: config.DNSModeAll, ToNS: "10.6.6.6:5353"}); !reflect.DeepEqual(got, []string{"--dns", "--to-ns", "10.6.6.6:5353"}) {
		t.Fatalf("DNSArgs(all+to-ns) = %q", got)
	}
	split := &config.DNSConfig{
		Mode:     config.DNSModeSplit,
		Resolver: []string{"10.0.0.2", "10.1.0.2"},
	}
	if got := DNSArgs(split); !reflect.DeepEqual(got, []string{"--ns-hosts", "10.0.0.2,10.1.0.2"}) {
		t.Fatalf("DNSArgs(split) = %q", got)
	}
	split.ToNS = "10.1.0.2"
	if got := DNSArgs(split); !reflect.DeepEqual(got, []string{"--ns-hosts", "10.0.0.2,10.1.0.2", "--to-ns", "10.1.0.2"}) {
		t.Fatalf("DNSArgs(split+to-ns) = %q", got)
	}
}

func TestBuildArgsFullDNSDefaultsToVPCResolver(t *testing.T) {
	cfg := &config.Config{
		Target: "i-0123456789abcdef0", VpcCIDRs: []string{"172.31.0.0/16"},
		Routes: []config.NetworkRoute{{CIDR: "172.31.0.0/16", Selected: true}},
		DNS:    &config.DNSConfig{Mode: config.DNSModeAll},
	}
	want := []string{"-r", "i-0123456789abcdef0", "172.31.0.0/16", "--dns", "--to-ns", "172.31.0.2"}
	if got := BuildArgs(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgs(all DNS) = %q, want %q", got, want)
	}
	cfg.DNS.Resolver = []string{"10.20.0.53"}
	want[len(want)-1] = "10.20.0.53"
	if got := BuildArgs(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgs(all DNS with resolver field) = %q, want %q", got, want)
	}
	cfg.DNS.ToNS = "1.1.1.1"
	want[len(want)-1] = "1.1.1.1"
	if got := BuildArgs(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgs(all DNS with explicit forwarder) = %q, want %q", got, want)
	}
}

func TestBuildArgsAppendsDNSFlagsAfterSubnets(t *testing.T) {
	cfg := &config.Config{
		Target:     "i-0123456789abcdef0",
		VpcCIDRs:   []string{"10.0.0.0/16"},
		SSHKeyPath: "",
		SSHPort:    22,
		DNS: &config.DNSConfig{
			Mode:     config.DNSModeSplit,
			Resolver: []string{"10.0.0.2"},
			Domains:  []string{"corp.internal"},
			ToNS:     "10.0.0.2",
		},
	}
	want := []string{
		"-r", "i-0123456789abcdef0",
		"10.0.0.0/16",
		"--ns-hosts", "10.0.0.2",
		"--to-ns", "10.0.0.2",
	}
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
	// /bin/sh, not /bin/sleep: on newer runner images (e.g. Ubuntu 26.04's
	// uutils coreutils) /bin/sleep dispatches on argv[0], and our "sshuttle"
	// symlink would make it exit instantly. sh -c keeps a stable sleeper;
	// the "|| true" stops sh from exec-replacing itself (which would hide
	// the sshuttle argv[0] from the ps inspection).
	if err := os.Symlink("/bin/sh", sshuttlePath); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(sshuttlePath, "-c", "sleep 30 || true")
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
