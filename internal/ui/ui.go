// Package ui renders the candidate table and handles the small amount of
// interactive prompting bastionctl needs (selection, ssh user/key, confirm).
package ui

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"bastionctl/internal/probe"
)

var stdin = bufio.NewReader(os.Stdin)

const (
	tablePadding = 2  // spaces between columns, also used as tabwriter's padding
	minNameWidth = 12 // never shrink the Name column below this, even on tiny terminals
)

// truncate shortens s to at most width characters, appending an ellipsis
// if it had to cut anything.
func truncate(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "\u2026"
}

// terminalWidth returns the current terminal column count and whether
// stdout is actually an interactive terminal. When it isn't (piped or
// redirected output), there's no line-wrapping concern, so callers should
// not truncate anything.
func terminalWidth() (int, bool) {
	fd := int(os.Stdout.Fd())
	if !term.IsTerminal(fd) {
		return 0, false
	}
	w, _, err := term.GetSize(fd)
	if err != nil || w <= 0 {
		return 0, false
	}
	return w, true
}

// colWidth returns the widest cell in a column, including its header.
func colWidth(header string, values []string) int {
	w := len(header)
	for _, v := range values {
		if len(v) > w {
			w = len(v)
		}
	}
	return w
}

// PrintCandidateTable prints the ranked candidate list to stdout using
// text/tabwriter for column alignment. Names are shown in full unless the
// terminal is too narrow to fit the whole row -- only then is just enough
// trimmed (with an ellipsis) to make it fit. Output to a non-terminal
// (piped/redirected) is never truncated.
func PrintCandidateTable(candidates []probe.Candidate) {
	fmt.Println()

	idxes := make([]string, len(candidates))
	instances := make([]string, len(candidates))
	names := make([]string, len(candidates))
	targets := make([]string, len(candidates))
	methods := make([]string, len(candidates))
	statuses := make([]string, len(candidates))
	networks := make([]string, len(candidates))
	vpcNets := make([]string, len(candidates))
	banners := make([]string, len(candidates))

	for i, c := range candidates {
		status := "unreachable"
		switch {
		case c.Method == probe.MethodSSM:
			status = "SSM online"
		case c.Reachable && strings.HasPrefix(c.Banner, "SSH-"):
			status = "SSH confirmed"
		case c.Reachable:
			status = "TCP open"
		}
		method := "-"
		if c.Method != "" {
			method = c.Method
		}
		name := c.Name
		if name == "" {
			name = "-"
		}
		target := c.Target
		if target == "" {
			target = "-"
		}
		vpcNet := "-"
		if c.Reachable {
			if c.VpcEgressOK {
				vpcNet = "OK"
			} else {
				vpcNet = "RESTRICTED"
			}
		}

		idxes[i] = strconv.Itoa(i + 1)
		instances[i] = c.InstanceID
		names[i] = name
		targets[i] = target
		methods[i] = method
		statuses[i] = status
		networks[i] = strings.Join(c.VpcCIDRs, ",")
		if networks[i] == "" {
			networks[i] = "-"
		}
		vpcNets[i] = vpcNet
		banners[i] = c.Banner
	}

	idxW := colWidth("#", idxes)
	instanceW := colWidth("Instance", instances)
	nameNaturalW := colWidth("Name", names)
	targetW := colWidth("Target", targets)
	methodW := colWidth("Method", methods)
	statusW := colWidth("Status", statuses)
	networkW := colWidth("Networks", networks)
	vpcNetW := colWidth("VPC net", vpcNets)

	// Name is the only column we ever shrink. Everything else keeps its
	// natural width so the table stays fully readable for the columns
	// that matter most for correctness (IDs, targets, status).
	nameW := nameNaturalW
	if width, isTTY := terminalWidth(); isTTY {
		// 8 gaps between the 9 columns (#, Instance, Name, Target,
		// Method, Status, Networks, VPC net, Banner), each padded by tablePadding
		// spaces below. Banner is excluded from "reserved" since it's
		// the last column and can wrap/overflow without breaking
		// alignment of anything before it.
		reserved := idxW + instanceW + targetW + methodW + statusW + networkW + vpcNetW + 8*tablePadding
		available := width - reserved
		if available < nameNaturalW {
			if available < minNameWidth {
				available = minNameWidth
			}
			nameW = available
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, tablePadding, tablePadding, ' ', 0)
	fmt.Fprintln(w, "#\tInstance\tName\tTarget\tMethod\tStatus\tNetworks\tVPC net\tBanner")
	for i := range candidates {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			idxes[i], instances[i], truncate(names[i], nameW), targets[i],
			methods[i], statuses[i], networks[i], vpcNets[i], banners[i])
	}
	w.Flush()

	// Printed separately below the table (rather than inline per row) so
	// these free-form warning lines -- which have no tab cells -- don't
	// reset tabwriter's column-width block partway through the table.
	for i, c := range candidates {
		if c.Reachable && !c.VpcEgressOK && len(c.VpcEgressGaps) > 0 {
			fmt.Printf("  #%d -> egress does not cover: %s (sshuttle may not reach these ranges through this box)\n",
				i+1, strings.Join(c.VpcEgressGaps, ", "))
		}
	}
	fmt.Println()
}

// PromptSelect asks the user to pick a candidate number, re-prompting on
// invalid input. Returns 1-based index.
func PromptSelect(max int, def int) int {
	for {
		fmt.Printf("Select candidate [1-%d] (default %d): ", max, def)
		line, _ := stdin.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > max {
			fmt.Println("Invalid selection, try again.")
			continue
		}
		return n
	}
}

// PromptString asks for a free-text value with a default.
func PromptString(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := stdin.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// PromptYesNo asks a yes/no question with a default.
func PromptYesNo(label string, def bool) bool {
	defStr := "Y/n"
	if !def {
		defStr = "y/N"
	}
	fmt.Printf("%s [%s]: ", label, defStr)
	line, _ := stdin.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

// PrintInstallHint tells the user how to install sshuttle when it's
// missing from PATH.
func PrintInstallHint() {
	fmt.Println("sshuttle was not found on this system (PATH lookup failed). Install it, e.g.:")
	fmt.Println("  macOS:          brew install sshuttle")
	fmt.Println("  Debian/Ubuntu:  sudo apt install sshuttle")
	fmt.Println("  Other:          pip install --user sshuttle")
}
