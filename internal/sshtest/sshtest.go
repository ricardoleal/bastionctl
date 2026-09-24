// Package sshtest verifies real end-to-end SSH connectivity by shelling
// out to the operating system's own `ssh` client, rather than reimplementing
// SSH or Session Manager tunneling. This means it transparently honors
// whatever the operator already has in ~/.ssh/config -- including a
// ProxyCommand that routes through AWS Systems Manager Session Manager
// (e.g. `Host i-* mi-*` patterns calling `aws ssm start-session ...`),
// a custom IdentityAgent, or a User directive -- without bastionctl
// needing to know anything about that setup.
package sshtest

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

// TryConnect attempts `ssh <host> true` with a bounded timeout and no
// interactive prompts (BatchMode=yes), returning whether it succeeded and
// the combined stdout/stderr for diagnostics on failure. host may be an
// IP address or, for SSM-brokered access, an EC2/managed-instance ID
// (e.g. "i-0123456789abcdef0") that matches a Host pattern in the
// operator's ~/.ssh/config.
func TryConnect(host string, port int, timeout time.Duration) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()

	args := []string{
		"-o", "ConnectTimeout=" + strconv.Itoa(int(timeout.Seconds())),
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
	}
	if port != 0 && port != 22 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args, host, "true")

	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}
