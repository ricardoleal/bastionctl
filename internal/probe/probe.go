// Package probe performs empirical reachability testing of candidate
// instances: a plain TCP connect to the SSH port, plus a best-effort read
// of the unauthenticated SSH identification banner. This replaces
// inference from Security Group rules (which can reference other SGs,
// prefix lists, or ranges unrelated to the operator's actual network
// path) with a direct, authoritative test.
package probe

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Connection methods a candidate can be reached by.
const (
	MethodSSM      = "ssm"       // AWS Systems Manager Session Manager (instance-id based, no network path needed)
	MethodDirectIP = "direct_ip" // Direct TCP to a public or private IP
)

// Candidate is an EC2 instance being evaluated as a possible sshuttle pivot.
type Candidate struct {
	InstanceID string
	Name       string
	VpcID      string
	VpcCIDRs   []string
	SubnetID   string
	PublicIP   string
	PrivateIP  string

	// Populated by ProbeAll (direct TCP fallback signal) and then
	// finalized by the caller once SSM status is merged in:
	Target    string // instance ID (ssm) or IP address (direct_ip)
	Method    string // MethodSSM or MethodDirectIP, empty if unreachable
	Reachable bool
	Banner    string // only meaningful for MethodDirectIP

	// VpcEgressOK reports whether this instance's own Security Group
	// egress rules allow it to reach every one of its VPC's CIDR blocks --
	// i.e. whether it can actually serve as a useful network pivot once
	// sshuttle is tunneling through it, as opposed to merely being
	// reachable itself. VpcEgressGaps lists any VPC CIDR blocks not
	// covered by an egress rule.
	VpcEgressOK   bool
	VpcEgressGaps []string
}

// ProbeSSH attempts a TCP connect to ip:port and, if successful, tries to
// read the SSH identification banner line. Returns (reachable, banner).
// An empty banner with reachable=true means the TCP port is open but no
// banner was captured in time (still useful signal, lower confidence).
func ProbeSSH(ip string, port int, timeout time.Duration) (bool, string) {
	if ip == "" {
		return false, ""
	}
	address := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false, ""
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return true, ""
	}
	return true, strings.TrimSpace(line)
}

// ProbeAll concurrently probes every candidate's public IP, falling back
// to its private IP if the public one is unreachable (or absent). This is
// a secondary/fallback signal to SSM reachability -- useful in VPCs that
// do have direct network access (public IP, VPN, peering). Results
// (Reachable/Banner/Target/Method=MethodDirectIP) are written back into
// the candidates slice in place; the caller should overlay SSM results
// afterwards, since SSM reachability takes priority when both apply.
func ProbeAll(candidates []Candidate, port int, timeout time.Duration) {
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func(c *Candidate) {
			defer wg.Done()
			if ok, banner := ProbeSSH(c.PublicIP, port, timeout); ok {
				c.Reachable, c.Banner, c.Target, c.Method = true, banner, c.PublicIP, MethodDirectIP
				return
			}
			if ok, banner := ProbeSSH(c.PrivateIP, port, timeout); ok {
				c.Reachable, c.Banner, c.Target, c.Method = true, banner, c.PrivateIP, MethodDirectIP
				return
			}
		}(&candidates[i])
	}
	wg.Wait()
}

// Rank returns a sort key, lower sorts first (best candidates on top).
// Reachability method is the primary key:
//
//	0-1 = reachable via SSM Session Manager (works regardless of network path)
//	2-3 = reachable via direct TCP with SSH banner confirmed
//	4-5 = reachable via direct TCP, port open but no banner captured
//	6-7 = unreachable
//
// Within each reachability tier, candidates whose Security Group egress
// covers the full VPC CIDR (VpcEgressOK) sort ahead of those with gaps --
// reachable-but-can't-route-anywhere-useful is a worse pivot choice.
func Rank(c Candidate) int {
	var base int
	switch {
	case c.Method == MethodSSM:
		base = 0
	case c.Reachable && strings.HasPrefix(c.Banner, "SSH-"):
		base = 1
	case c.Reachable:
		base = 2
	default:
		base = 3
	}
	if c.VpcEgressOK {
		return base * 2
	}
	return base*2 + 1
}
