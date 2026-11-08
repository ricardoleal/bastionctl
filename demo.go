// demo.go runs the real interactive browser against fully simulated data so
// the k9s-style UI can be demoed/recorded without any AWS access:
//
//	./bastionctl --demo
//
// The `--demo` flag is intentionally hidden from --help output. Actions work
// the same way they do against a real cache — the executor below mutates the
// in-memory mock state and prints plausible action output — but no files, pid
// files, or resolver configs are ever touched.
package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"

	"bastionctl/internal/config"
	ui "bastionctl/internal/ui"
)

// demoConn is one simulated connection's mutable state.
type demoConn struct {
	Account     string
	Name        string
	Kind        string
	Method      string
	Region      string
	AWSProfile  string
	Networks    []string
	Selected    []string
	DNSSummary  string
	Domains     []string
	Resolver    string
	Split       bool
	Running     bool
	PID         int
	Unreachable bool
	HasResolver bool // state manifest exists for split DNS
	Target      string
}

func (conn *demoConn) AWSProfileLabel() string {
	if conn.AWSProfile == "" {
		return "default"
	}
	return conn.AWSProfile
}

func demoConnections() []*demoConn {
	return []*demoConn{
		{
			Account: "123456789012", Name: "dns-split", Kind: "vpc", Method: "ssm",
			Region:     "eu-central-1",
			Networks:   []string{"10.0.0.0/16", "172.31.0.0/16", "10.1.0.0/24"},
			Selected:   []string{"10.0.0.0/16", "172.31.0.0/16"},
			Split:      true,
			Domains:    []string{"company.internal", "svc.cluster.local"},
			Resolver:   "172.31.0.2",
			DNSSummary: "split (2 domain(s) via 172.31.0.2)",
			Running:    true, PID: 2431, HasResolver: true,
			Target: "i-00de18e4510b445fa",
		},
		{
			Account: "123456789012", Name: "corp", Kind: "vpc", Method: "ssh",
			Region:   "eu-west-1",
			Networks: []string{"172.16.0.0/12"},
			Selected: []string{"172.16.0.0/12"},
			Target:   "10.16.7.19",
		},
		{
			Account: "014498740021", Name: "legacy-app", Kind: "vpc", Method: "ssm",
			Region:     "us-east-1",
			Networks:   []string{"10.20.0.0/16"},
			Selected:   []string{"10.20.0.0/16"},
			Split:      true,
			Domains:    []string{"legacy.internal"},
			Resolver:   "10.20.0.2",
			DNSSummary: "split (1 domain(s) via 10.20.0.2)",
			Running:    true, PID: 8871,
			Target: "i-09a7d21b0c4e3f2a1",
		},
		{
			Account: "014498740021", Name: "ci-edge", Kind: "dedicated", Method: "ssh",
			Region:      "us-west-2",
			Networks:    []string{"10.90.0.0/24"},
			Selected:    []string{"10.90.0.0/24"},
			Unreachable: true,
			Target:      "3.14.159.26",
		},
	}
}

const demoAccountLabel = "123456789012 (prod) · 014498740021 (staging) · demo"

var demoRegions = []string{"eu-central-1", "eu-west-1", "us-east-1", "us-west-2"}

// runDemoApp runs the browser against the mock state until the user quits.
func runDemoApp() {
	fmt.Fprintln(os.Stderr, "Demo mode: simulated connections; nothing touches your config or resolver files.")
	conns := demoConnections()
	level := 0
	profileName := ""
	highlight := 0
	pendingError := ""

	for {
		summaries := demoSummaries(conns)
		names := make([]string, 0, len(conns))
		for _, conn := range conns {
			names = append(names, conn.Name)
		}
		if profileName != "" {
			highlight = 0
			for index, name := range names {
				if name == profileName {
					highlight = index
					break
				}
			}
			if level == 1 && highlight == 0 && names[0] != profileName {
				level = 0
				profileName = ""
			}
		}

		var detail *ui.ConnDetail
		if level == 1 {
			detail = demoDetail(findDemoConn(conns, profileName))
			if detail == nil {
				level = 0
				profileName = ""
			}
		}

		outcome, err := ui.RunApp(ui.RunOptions{
			Account:   demoAccountLabel,
			Error:     pendingError,
			Level:     level,
			Highlight: highlight,
			Summaries: summaries,
			Detail:    detail,
			LoadDetail: func(name string) (*ui.ConnDetail, error) {
				detail := demoDetail(findDemoConn(conns, name))
				if detail == nil {
					return nil, fmt.Errorf("connection %q is no longer available", name)
				}
				return detail, nil
			},
			LoadConfig: func(name string) (*config.Config, error) {
				conn := findDemoConn(conns, name)
				if conn == nil {
					return nil, fmt.Errorf("connection %q is no longer available", name)
				}
				return demoConfig(conn), nil
			},
			SaveConfig: func(name string, cfg *config.Config) error {
				conn := findDemoConn(conns, name)
				if conn == nil {
					return fmt.Errorf("connection %q is no longer available", name)
				}
				applyDemoConfig(conn, cfg)
				return nil
			},
			AWSProfiles: []string{"default"},
			AWSRegions:  demoRegions,
			CheckAWSProfile: func(cfg *config.Config, profile string) (ui.ActionResult, error) {
				return ui.ActionResult{Title: "AWS profile readiness", Body: fmt.Sprintf("Demo mode does not verify profile %q against AWS.", profile)}, nil
			},
			RefreshConnection: func(name string) (ui.ConnSummary, *ui.ConnDetail, error) {
				conn := findDemoConn(conns, name)
				if conn == nil {
					return ui.ConnSummary{}, nil, fmt.Errorf("connection %q is no longer available", name)
				}
				return demoSummary(conn), demoDetail(conn), nil
			},
			Actions: demoActionHandlers(&conns),
			StartLogs: func(name string) (*ui.LogSession, error) {
				if findDemoConn(conns, name) == nil {
					return nil, fmt.Errorf("connection %q is no longer available", name)
				}
				lines := make(chan string, 3)
				lines <- "demo: sshuttle log stream"
				lines <- "demo: connection " + name + " is healthy"
				close(lines)
				done := make(chan error, 1)
				done <- nil
				close(done)
				return &ui.LogSession{Lines: lines, Done: done, Cancel: func() {}}, nil
			},
			StartLaunch: func(name string) (*ui.LogSession, error) {
				conn := findDemoConn(conns, name)
				if conn == nil {
					return nil, fmt.Errorf("connection %q is no longer available", name)
				}
				conn.Running = false
				lines := make(chan string, 2)
				lines <- "demo: launching " + demoCommand(conn)
				lines <- "demo: foreground tunnel exited"
				close(lines)
				done := make(chan error, 1)
				done <- nil
				close(done)
				return &ui.LogSession{Lines: lines, Done: done, Cancel: func() {}}, nil
			},
			PrepareRescan: func(name string) ([]ui.RescanCandidate, error) {
				conn := findDemoConn(conns, name)
				if conn == nil {
					return nil, fmt.Errorf("connection %q is no longer available", name)
				}
				return []ui.RescanCandidate{
					{Name: "alternate pivot", InstanceID: "i-demo-alternate", Method: conn.Method, Target: "i-demo-alternate", VpcID: "vpc-demo", VpcCIDRs: conn.Networks, SubnetID: "subnet-demo", Reachable: true, EgressOK: true},
					{Name: conn.Name, InstanceID: conn.Target, Method: conn.Method, Target: conn.Target, VpcID: "vpc-demo", VpcCIDRs: conn.Networks, SubnetID: "subnet-demo", Reachable: true, EgressOK: true, Current: true},
				}, nil
			},
			SaveRescan: func(name string, candidate ui.RescanCandidate) (*config.Config, error) {
				conn := findDemoConn(conns, name)
				if conn == nil {
					return nil, fmt.Errorf("connection %q is no longer available", name)
				}
				conn.Target, conn.Method, conn.Unreachable = candidate.Target, candidate.Method, false
				cfg := demoConfig(conn)
				cfg.InstanceID, cfg.SubnetID, cfg.VpcID, cfg.VpcCIDRs = candidate.InstanceID, candidate.SubnetID, candidate.VpcID, candidate.VpcCIDRs
				return cfg, nil
			},
			StartNew: func() (ui.NewConnectionStart, error) {
				return ui.NewConnectionStart{Regions: demoRegions, AccountID: "123456789012", AWSProfile: "default", SSHPort: 22}, nil
			},
			ScanNew: func(region, kind string) ([]ui.RescanCandidate, error) {
				return demoScanCandidates(region, kind), nil
			},
			PrepareNewRoutes: func(_, _ string, candidate ui.RescanCandidate) ([]config.NetworkRoute, error) {
				return demoRoutes(candidate.VpcCIDRs), nil
			},
			SaveNewConfig: func(cfg *config.Config) error {
				for _, conn := range conns {
					if conn.Account == cfg.AccountID && conn.Name == cfg.Name {
						return fmt.Errorf("connection %q already exists", cfg.Name)
					}
				}
				conn := &demoConn{}
				applyDemoConfig(conn, cfg)
				conns = append(conns, conn)
				return nil
			},
		})
		pendingError = ""
		if err != nil {
			fatal(err)
		}
		if outcome.Quit {
			return
		}
		if outcome.Action == "" {
			level = 0
			profileName = ""
			continue
		}
		if outcome.Action == ui.ActOpen {
			level = 1
			profileName = outcome.Connection
			continue
		}
		pendingError = fmt.Sprintf("Action %q has no in-app demo handler", outcome.Action)
	}
}

func findDemoConn(conns []*demoConn, name string) *demoConn {
	for _, conn := range conns {
		if conn.Name == name {
			return conn
		}
	}
	return nil
}

// demoSummaries derives the home-table rows; the stale flag mirrors the
// real logic (running + split + auto-managed resolver expected but missing).
func demoSummaries(conns []*demoConn) []ui.ConnSummary {
	summaries := make([]ui.ConnSummary, 0, len(conns))
	for _, conn := range conns {
		summary := ui.ConnSummary{
			Account:     conn.Account,
			Name:        conn.Name,
			Kind:        conn.Kind,
			Method:      conn.Method,
			Region:      conn.Region,
			AWSProfile:  conn.AWSProfile,
			Networks:    joinList(conn.Selected),
			DNS:         orOff(conn.DNSSummary),
			Running:     conn.Running,
			PID:         conn.PID,
			Unreachable: conn.Unreachable && !conn.Running,
		}
		if conn.Running && conn.Split && !conn.HasResolver {
			summary.Stale = true
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i int, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries
}

// demoDetail builds the Level-1 card payload; reachability follows the
// mock state (offline sim: checked state comes from summary flags).
func demoDetail(conn *demoConn) *ui.ConnDetail {
	if conn == nil {
		return nil
	}
	routes := make([]config.NetworkRoute, 0, len(conn.Networks))
	for _, network := range conn.Networks {
		route := config.NetworkRoute{CIDR: network, Source: "pivot_vpc"}
		for _, chosen := range conn.Selected {
			if chosen == network {
				route.Selected = true
				break
			}
		}
		routes = append(routes, route)
	}
	detail := &ui.ConnDetail{
		Name:       conn.Name,
		Kind:       conn.Kind,
		Method:     conn.Method,
		Region:     conn.Region,
		AWSProfile: conn.AWSProfile,
		SSHPort:    22,
		Running:    conn.Running,
		PID:        conn.PID,
		Target:     conn.Target,
		Domains:    conn.Domains,
		Routes:     routes,
		Networks:   conn.Selected,
		Command:    demoCommand(conn),
		DNSSummary: conn.DNSSummary,
		TargetOK:   !conn.Unreachable,
	}
	if conn.Split && conn.HasResolver {
		detail.Resolver = []string{conn.Resolver}
		detail.DNSInstalled = fmt.Sprintf("resolver files: /etc/resolver/%s", conn.Domains[0])
	} else if conn.Split {
		detail.DNSWantsInstall = true
	}
	if !conn.Running && conn.Unreachable {
		detail.ReachDetail = "connection refused"
	} else if conn.Running {
		detail.TargetOK = true
	}
	return detail
}

// demoScanCandidates simulates pivot discovery; no AWS calls are made.
func demoScanCandidates(region, kind string) []ui.RescanCandidate {
	method := "ssm"
	if kind == string(config.ProfileKindDedicated) {
		method = "ssh"
	}
	return []ui.RescanCandidate{
		{Name: "demo pivot a (" + region + ")", InstanceID: "i-demo-a", Method: method, Target: "i-demo-a", VpcID: "vpc-demo", VpcCIDRs: []string{"10.50.0.0/16"}, SubnetID: "subnet-demo-a", Reachable: true, EgressOK: true},
		{Name: "demo pivot b (" + region + ")", InstanceID: "i-demo-b", Method: method, Target: "i-demo-b", VpcID: "vpc-demo", VpcCIDRs: []string{"10.50.0.0/16", "172.20.0.0/16"}, SubnetID: "subnet-demo-b", Reachable: true, EgressOK: false},
	}
}

// demoRoutes marks the first discovered CIDR as selected by default.
func demoRoutes(cidrs []string) []config.NetworkRoute {
	routes := make([]config.NetworkRoute, 0, len(cidrs))
	for index, cidr := range cidrs {
		routes = append(routes, config.NetworkRoute{CIDR: cidr, Source: "demo", Selected: index == 0})
	}
	return routes
}

func joinList(items []string) string {
	return strings.Join(items, ", ")
}

func orOff(value string) string {
	if value == "" {
		return "off"
	}
	return value
}

func demoCommand(conn *demoConn) string {
	preview := "sshuttle -r " + conn.Target + " " + joinList(conn.Selected)
	if conn.Split {
		preview += " --ns-hosts " + conn.Resolver
	}
	return preview
}

func demoSummary(conn *demoConn) ui.ConnSummary {
	return ui.ConnSummary{
		Account: conn.Account, Name: conn.Name, Kind: conn.Kind, Method: conn.Method,
		Region: conn.Region, AWSProfile: conn.AWSProfile, Networks: joinList(conn.Selected),
		DNS: orOff(conn.DNSSummary), Running: conn.Running, PID: conn.PID,
		Unreachable: conn.Unreachable && !conn.Running,
		Reachable:   !conn.Unreachable,
		Stale:       conn.Running && conn.Split && !conn.HasResolver,
	}
}

func demoConfig(conn *demoConn) *config.Config {
	routes := make([]config.NetworkRoute, 0, len(conn.Networks))
	for _, network := range conn.Networks {
		selected := false
		for _, current := range conn.Selected {
			if network == current {
				selected = true
				break
			}
		}
		routes = append(routes, config.NetworkRoute{CIDR: network, Source: "demo", Selected: selected})
	}
	cfg := &config.Config{
		Name: conn.Name, AccountID: conn.Account, AWSProfile: conn.AWSProfile,
		Kind: config.ProfileKind(conn.Kind), Region: conn.Region, VpcID: "vpc-demo",
		VpcCIDRs: append([]string(nil), conn.Networks...), InstanceID: conn.Target,
		NameTag: conn.Name, Routes: routes, ConnectionMethod: conn.Method,
		Target: conn.Target, SSHPort: 22,
	}
	if conn.Split {
		cfg.DNS = &config.DNSConfig{Mode: config.DNSModeSplit, Domains: append([]string(nil), conn.Domains...), Resolver: []string{conn.Resolver}, ManageResolver: conn.HasResolver}
	}
	return cfg
}

func applyDemoConfig(conn *demoConn, cfg *config.Config) {
	conn.Name, conn.Kind, conn.Method, conn.Region = cfg.Name, string(cfg.Kind), cfg.ConnectionMethod, cfg.Region
	conn.Account, conn.AWSProfile, conn.Target = cfg.AccountID, cfg.AWSProfile, cfg.Target
	conn.Networks = append([]string(nil), cfg.VpcCIDRs...)
	conn.Selected = conn.Selected[:0]
	for _, route := range cfg.Routes {
		conn.Networks = appendUniqueString(conn.Networks, route.CIDR)
		if route.Selected {
			conn.Selected = append(conn.Selected, route.CIDR)
		}
	}
	conn.Split = cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit
	conn.DNSSummary = ""
	if conn.Split {
		conn.Domains = append([]string(nil), cfg.DNS.Domains...)
		conn.Resolver = strings.Join(cfg.DNS.Resolver, ", ")
		conn.DNSSummary = cfg.DNSSummary()
	}
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func demoActionHandlers(conns *[]*demoConn) map[string]ui.ActionHandler {
	find := func(name string) *demoConn { return findDemoConn(*conns, name) }
	return map[string]ui.ActionHandler{
		ui.ActStart: func(name string) (ui.ActionResult, error) {
			conn := find(name)
			if conn == nil {
				return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
			}
			if !conn.Running {
				conn.PID = 1000 + rand.IntN(8000)
				conn.Running = true
				conn.Unreachable = false
			}
			return ui.ActionResult{Title: "Tunnel started", Body: fmt.Sprintf("%s is running (PID %d).", name, conn.PID), Refresh: true}, nil
		},
		ui.ActStop: func(name string) (ui.ActionResult, error) {
			conn := find(name)
			if conn == nil {
				return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
			}
			conn.Running, conn.HasResolver = false, false
			return ui.ActionResult{Title: "Tunnel stopped", Body: name + " is stopped.", Refresh: true}, nil
		},
		ui.ActForget: func(name string) (ui.ActionResult, error) {
			for index, conn := range *conns {
				if conn.Name == name {
					*conns = append((*conns)[:index], (*conns)[index+1:]...)
					return ui.ActionResult{Title: "Connection forgotten", Body: name + " was removed from the demo.", RemoveConnection: true}, nil
				}
			}
			return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
		},
		ui.ActStatus: func(name string) (ui.ActionResult, error) {
			conn := find(name)
			if conn == nil {
				return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
			}
			status := "Not running"
			if conn.Running {
				status = fmt.Sprintf("Running (PID %d)", conn.PID)
			} else if conn.Unreachable {
				status = "Not reachable"
			}
			return ui.ActionResult{Title: "Connection status", Body: name + ": " + status}, nil
		},
		ui.ActPrint: func(name string) (ui.ActionResult, error) {
			conn := find(name)
			if conn == nil {
				return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
			}
			return ui.ActionResult{Title: "sshuttle command", Body: demoCommand(conn)}, nil
		},
		ui.ActDNSSetup: func(name string) (ui.ActionResult, error) {
			conn := find(name)
			if conn == nil {
				return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
			}
			if !conn.Split {
				return ui.ActionResult{}, fmt.Errorf("%s has no split-DNS settings", name)
			}
			conn.HasResolver = true
			return ui.ActionResult{Title: "Split DNS installed", Body: conn.DNSSummary, Refresh: true}, nil
		},
		ui.ActDNSTeardown: func(name string) (ui.ActionResult, error) {
			conn := find(name)
			if conn == nil {
				return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
			}
			conn.HasResolver = false
			return ui.ActionResult{Title: "Split DNS removed", Body: name + " resolver settings removed in demo.", Refresh: true}, nil
		},
		ui.ActAWSCheck: func(name string) (ui.ActionResult, error) {
			conn := find(name)
			if conn == nil {
				return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
			}
			return ui.ActionResult{Title: "AWS profile readiness", Body: fmt.Sprintf("Demo mode does not verify profile %q against AWS.", conn.AWSProfileLabel())}, nil
		},
	}
}

// demoExecute applies one outcome to the mock state and prints plausible
// action output, mirroring the real single-shot actions' wording. It
// reports whether the connection was forgotten (removed from the list).
func demoExecute(conn *demoConn, action string) bool {
	if conn == nil {
		return false
	}
	switch action {
	case ui.ActStart:
		if conn.Running {
			fmt.Printf("[Running] %s (PID %d)\n", conn.Name, conn.PID)
			break
		}
		fmt.Printf("[Starting] %s\n", conn.Name)
		conn.PID = 1000 + rand.IntN(8000)
		conn.Running = true
		conn.Unreachable = false
		fmt.Printf("[Running] %s (PID %d)\n", conn.Name, conn.PID)
		fmt.Println("PID file: (demo) ~/.bastionctl/" + conn.Name + ".pid")
		fmt.Println("Live logs: bastionctl --logs")
	case ui.ActLaunch:
		fmt.Printf("[Starting foreground] %s (demo: exits immediately)\n", conn.Name)
		fmt.Printf("[Not running] %s\n", conn.Name)
	case ui.ActStop:
		if !conn.Running {
			fmt.Printf("[Not running] %s\n", conn.Name)
			break
		}
		fmt.Printf("[Stopping] %s\n", conn.Name)
		conn.Running = false
		conn.HasResolver = false
		fmt.Printf("[Not running] %s (stopped PID %d)\n", conn.Name, conn.PID)
	case ui.ActStatus:
		if conn.Running {
			fmt.Printf("[Running] %s (PID %d)\n", conn.Name, conn.PID)
		} else if conn.Unreachable {
			fmt.Printf("[Not running] %s — target unreachable\n", conn.Name)
		} else {
			fmt.Printf("[Not running] %s\n", conn.Name)
		}
		if conn.Split && conn.Running && !conn.HasResolver {
			fmt.Println("DNS:", conn.DNSSummary)
			fmt.Println("WARNING: split-DNS resolver config is not installed; run :dns install")
		}
	case ui.ActLogs:
		fmt.Println("Following sshuttle daemon logs (demo). Press Ctrl+C to stop viewing logs.")
		fmt.Println("fw: adding ingress rule #5 on lo0 … (demo log line 1)")
		fmt.Println("fw: adding ingress rule #6 on lo0 … (demo log line 2)")
		fmt.Println("c: connected to server " + conn.Target)
	case ui.ActPrint:
		fmt.Println("\nReady to establish the tunnel. Run:")
		fmt.Println("  " + demoCommand(conn))
	case ui.ActDNSSetup:
		if !conn.Split {
			fmt.Println("No split-DNS configured for", conn.Name, "(demo: edit with 'e' first).")
			break
		}
		fmt.Println("Split-DNS resolver config installed.")
		fmt.Println("split DNS:", len(conn.Domains), "domain(s) via", conn.Resolver)
		fmt.Println("(demo: resolver files at /etc/resolver/" + conn.Domains[0] + ")")
		conn.HasResolver = true
	case ui.ActDNSTeardown:
		if !conn.Split || !conn.HasResolver {
			fmt.Println("No split-DNS resolver config installed for", conn.Name, "(demo).")
			break
		}
		fmt.Println("Split-DNS resolver config removed.")
		conn.HasResolver = false
	case ui.ActDNSEdit:
		fmt.Printf("Updated DNS settings for connection %q (mode: %s). (demo)\n", conn.Name, orOff(conn.DNSSummary))
	case ui.ActRescan:
		fmt.Println("Rescan complete (demo): same networks, same target.")
	case ui.ActForget:
		fmt.Println("Removed connection:", conn.Name)
		ui.Pause("Press Enter to return…")
		return true
	case "":
		// no-op
	}
	ui.Pause("Press Enter to return…")
	return false
}
