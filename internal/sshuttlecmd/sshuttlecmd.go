// Package sshuttlecmd locates the system's sshuttle binary and builds/
// prints/launches the tunnel command. It deliberately does NOT reimplement
// sshuttle's firewall/NAT manipulation -- that stays with the real
// sshuttle binary, which must already be installed on the operator's
// machine (it remains a Python tool; that's an unavoidable, separate
// runtime dependency of sshuttle itself, not of bastionctl).
package sshuttlecmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bastionctl/internal/config"
)

// FindBinary looks up sshuttle on PATH.
func FindBinary() (string, error) {
	return exec.LookPath("sshuttle")
}

// BuildArgs constructs the sshuttle argument list (excluding the binary
// name and any sudo prefix).
//
// For "ssm" targets, no --ssh-cmd override is emitted unless the user
// explicitly set a key path or non-default port: the whole point of the
// ssm method is that ~/.ssh/config already knows how to reach the target
// (e.g. via a `Host i-* mi-*` ProxyCommand through AWS Systems Manager
// Session Manager, with its own User/IdentityAgent directives), and we
// don't want to fight that configuration.
func BuildArgs(cfg *config.Config) []string {
	hostArg := cfg.Target
	if cfg.SSHUser != "" {
		hostArg = fmt.Sprintf("%s@%s", cfg.SSHUser, cfg.Target)
	}

	args := []string{"-r", hostArg}
	args = append(args, cfg.SelectedCIDRs()...)

	needsOverride := cfg.SSHKeyPath != "" || (cfg.SSHPort != 0 && cfg.SSHPort != 22)
	if needsOverride {
		sshParts := []string{"ssh"}
		if cfg.SSHKeyPath != "" {
			sshParts = append(sshParts, "-i", cfg.SSHKeyPath)
		}
		if cfg.SSHPort != 0 && cfg.SSHPort != 22 {
			sshParts = append(sshParts, "-p", strconv.Itoa(cfg.SSHPort))
		}
		args = append(args, "--ssh-cmd", strings.Join(sshParts, " "))
	}
	return args
}

// PrintCommand prints the full command line for the user to copy/run
// themselves (the default behavior -- see design notes on why we don't
// auto-launch by default).
func PrintCommand(bin string, args []string) {
	full := bin + " " + strings.Join(quoteArgs(args), " ")
	fmt.Println("\nReady to establish the tunnel. Run:")
	fmt.Println("  " + full)
}

// Launch executes sshuttle as the current user so its SSH child retains
// access to that user's config and agent. sshuttle elevates its own
// firewall helper when needed.
func Launch(bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// StartBackground asks sshuttle to daemonize itself after connection and
// firewall setup. Keeping sshuttle as the current user preserves access to
// that user's SSH config and agent; sshuttle elevates only its firewall helper.
func StartBackground(bin string, args []string, pidFile string) (int, error) {
	daemonArgs := buildDaemonArgs(args, pidFile)
	cmd := exec.Command(bin, daemonArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return 0, err
	}

	return waitForPIDFile(pidFile, 2*time.Second)
}

func waitForPIDFile(pidFile string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		pid, err := readPIDFile(pidFile)
		if err == nil {
			return pid, nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("reading sshuttle pid file %s: %w", pidFile, lastErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readPIDFile(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid contents in %s", pidFile)
	}
	return pid, nil
}

// BackgroundStatus validates the pidfile and reports whether it points to a
// live sshuttle daemon. Stale pidfiles are removed automatically.
func BackgroundStatus(pidFile string) (int, bool, error) {
	pid, err := readPIDFile(pidFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, false, err
	}
	alive, err := processAlive(process)
	if err != nil {
		return 0, false, err
	}
	if !alive {
		_ = os.Remove(pidFile)
		return pid, false, nil
	}
	command, err := processCommand(pid)
	if err != nil {
		return 0, false, err
	}
	if !isSSHuttleCommand(command) {
		return 0, false, fmt.Errorf("PID %d is not sshuttle (%s)", pid, strings.TrimSpace(command))
	}
	return pid, true, nil
}

// StopBackground sends SIGTERM to the sshuttle daemon recorded in pidFile.
// It verifies the process command first so a stale, reused PID cannot stop an
// unrelated process. The bool result reports whether a daemon was running.
func StopBackground(pidFile string) (int, bool, error) {
	pid, running, err := BackgroundStatus(pidFile)
	if err != nil || !running {
		return pid, false, err
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return 0, false, err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return 0, false, fmt.Errorf("stopping sshuttle PID %d: %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := processAlive(process)
		if err != nil {
			return 0, false, err
		}
		if !alive {
			_ = os.Remove(pidFile)
			return pid, true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, false, fmt.Errorf("sshuttle PID %d did not stop within 10 seconds", pid)
}

func processAlive(process *os.Process) (bool, error) {
	err := process.Signal(syscall.Signal(0))
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func processCommand(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", fmt.Errorf("inspecting PID %d: %w", pid, err)
	}
	return string(out), nil
}

func isSSHuttleCommand(command string) bool {
	for _, field := range strings.Fields(command) {
		if strings.EqualFold(filepath.Base(strings.Trim(field, `"'`)), "sshuttle") {
			return true
		}
	}
	return false
}

func buildDaemonArgs(args []string, pidFile string) []string {
	daemonArgs := []string{"--daemon", "--verbose", "--pidfile", pidFile}
	return append(daemonArgs, args...)
}

// FollowLogs shows recent sshuttle daemon logs and follows new entries.
// sshuttle's native daemon mode always redirects output through syslog.
func FollowLogs() error {
	commands, err := logCommands(runtime.GOOS)
	if err != nil {
		return err
	}
	for _, command := range commands {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}

func logCommands(goos string) ([][]string, error) {
	switch goos {
	case "darwin":
		predicate := `process == "logger" AND (eventMessage BEGINSWITH "c : " OR eventMessage BEGINSWITH "fw: " OR eventMessage BEGINSWITH "HH: ")`
		return [][]string{
			{"/usr/bin/log", "show", "--last", "5m", "--style", "compact", "--info", "--debug", "--predicate", predicate},
			{"/usr/bin/log", "stream", "--style", "compact", "--level", "debug", "--predicate", predicate},
		}, nil
	case "linux":
		command, err := linuxLogCommand(exec.LookPath, func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		})
		if err != nil {
			return nil, err
		}
		return [][]string{command}, nil
	default:
		return nil, fmt.Errorf("live sshuttle logs are not supported on %s", goos)
	}
}

func linuxLogCommand(lookPath func(string) (string, error), fileExists func(string) bool) ([]string, error) {
	if _, err := lookPath("journalctl"); err == nil {
		return []string{"journalctl", "--follow", "--identifier", "sshuttle", "--lines", "50"}, nil
	}
	for _, path := range []string{"/var/log/syslog", "/var/log/messages"} {
		if fileExists(path) {
			return []string{"tail", "-n", "50", "-F", path}, nil
		}
	}
	return nil, errors.New("neither journalctl nor a readable system log file is available")
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t") {
			out[i] = "\"" + a + "\""
		} else {
			out[i] = a
		}
	}
	return out
}
