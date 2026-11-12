// Package sshuttlecmd locates the system's sshuttle binary and builds/
// prints/launches the tunnel command. It deliberately does NOT reimplement
// sshuttle's firewall/NAT manipulation -- that stays with the real
// sshuttle binary, which must already be installed on the operator's
// machine (it remains a Python tool; that's an unavoidable, separate
// runtime dependency of sshuttle itself, not of bastionctl).
package sshuttlecmd

import (
	"bufio"
	"bytes"
	"context"
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
	args = append(args, DNSArgs(cfg.EffectiveDNSConfig())...)

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

// DNSArgs maps a connection's DNS settings onto sshuttle flags:
//
//	off:   nothing (historical behavior).
//	all:   --dns [ --to-ns IP[:PORT] ]  — capture every query to the local
//	       system resolvers and resolve on the remote side.
//	split: --ns-hosts IP[,IP] [ --to-ns IP[:PORT] ]  — capture only queries
//	       sent to the listed resolver IPs (the OS resolver is responsible
//	       for sending only the split domains to them).
func DNSArgs(d *config.DNSConfig) []string {
	if d == nil {
		return nil
	}
	switch d.EffectiveMode() {
	case config.DNSModeAll:
		args := []string{"--dns"}
		if d.ToNS != "" {
			args = append(args, "--to-ns", d.ToNS)
		}
		return args
	case config.DNSModeSplit:
		args := []string{"--ns-hosts", strings.Join(d.Resolver, ",")}
		if d.ToNS != "" {
			args = append(args, "--to-ns", d.ToNS)
		}
		return args
	default:
		return nil
	}
}

// PrintCommand prints the full command line for the user to copy/run
// themselves (the default behavior -- see design notes on why we don't
// auto-launch by default).
func PrintCommand(bin string, args []string) {
	full := CommandPreview(bin, args)
	fmt.Println("\nReady to establish the tunnel. Run:")
	fmt.Println("  " + full)
}

// CommandPreview returns the full sshuttle command as a copy-pasteable string.
func CommandPreview(bin string, args []string) string {
	return bin + " " + strings.Join(quoteArgs(args), " ")
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

// StartBackgroundCaptured starts sshuttle without inheriting the interactive
// terminal and returns any startup output for an in-app caller to display.
// environment entries override the parent process's variables for the child
// (e.g. AWS_REGION, so the SSM ProxyCommand targets the connection's region).
func StartBackgroundCaptured(ctx context.Context, bin string, args []string, pidFile string, environment map[string]string) (int, string, error) {
	cmd := exec.CommandContext(ctx, bin, buildDaemonArgs(args, pidFile)...)
	if environment != nil {
		cmd.Env = environmentWithOverrides(os.Environ(), environment)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return 0, output.String(), fmt.Errorf("starting sshuttle: %w", err)
	}
	pid, err := waitForPIDFile(pidFile, 2*time.Second)
	if err != nil {
		return 0, output.String(), err
	}
	return pid, output.String(), nil
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
		// The process may have exited while it was being inspected (a
		// momentary corpse: ps shows it bracketed or not at all while the
		// liveness check still saw it). Verify once more before declaring
		// the PID foreign; a dead process is just a stale PID file.
		if alive, err := processAlive(process); err == nil && !alive {
			_ = os.Remove(pidFile)
			return pid, false, nil
		}
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
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// ps exits 1 when the process no longer exists: it died between
			// the liveness check and this inspection. Report nothing; the
			// caller re-checks liveness and treats it as a stale PID file.
			return "", nil
		}
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

// LogSession captures sshuttle logs without taking ownership of the terminal.
type LogSession struct {
	Lines  <-chan string
	Done   <-chan error
	Cancel func()
}

// StartLogStream starts the platform log commands and streams complete lines.
func StartLogStream() (*LogSession, error) {
	commands, err := logCommands(runtime.GOOS)
	if err != nil {
		return nil, err
	}
	lines := make(chan string)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(lines)
		var runErr error
		for _, command := range commands {
			cmd := exec.CommandContext(ctx, command[0], command[1:]...)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				runErr = err
				break
			}
			cmd.Stderr = cmd.Stdout
			if err := cmd.Start(); err != nil {
				runErr = err
				break
			}
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				select {
				case lines <- scanner.Text():
				case <-ctx.Done():
					runErr = ctx.Err()
				}
				if runErr != nil {
					break
				}
			}
			if err := scanner.Err(); err != nil && runErr == nil {
				runErr = err
			}
			waitErr := cmd.Wait()
			if runErr == nil && waitErr != nil && ctx.Err() == nil {
				runErr = waitErr
			}
			if runErr != nil || ctx.Err() != nil {
				break
			}
		}
		if errors.Is(runErr, context.Canceled) {
			runErr = nil
		}
		done <- runErr
		close(done)
	}()
	return &LogSession{Lines: lines, Done: done, Cancel: cancel}, nil
}

// StartCommandStream runs a foreground command without attaching it to the
// terminal and streams combined output to the caller.
func StartCommandStream(ctx context.Context, bin string, args []string, environment map[string]string) *LogSession {
	lines := make(chan string)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(lines)
		cmd := exec.CommandContext(ctx, bin, args...)
		if environment != nil {
			cmd.Env = environmentWithOverrides(os.Environ(), environment)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			done <- err
			close(done)
			return
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			done <- err
			close(done)
			return
		}
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
		}
		err = scanner.Err()
		waitErr := cmd.Wait()
		if err == nil && waitErr != nil && ctx.Err() == nil {
			err = waitErr
		}
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		done <- err
		close(done)
	}()
	return &LogSession{Lines: lines, Done: done, Cancel: cancel}
}

func environmentWithOverrides(environment []string, overrides map[string]string) []string {
	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			result = append(result, entry)
			continue
		}
		if _, overridden := overrides[key]; !overridden {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
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
