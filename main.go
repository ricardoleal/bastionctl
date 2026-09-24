// bastionctl discovers, tests, and remembers the best EC2 instance to use
// as an sshuttle pivot into an AWS VPC.
//
// Reachability is determined two ways: (1) AWS Systems Manager
// DescribeInstanceInformation, which reports instances reachable via
// Session Manager regardless of network path/public IP/Security Group
// (the primary signal in environments that broker SSH through
// `aws ssm start-session`, matched by e.g. a `Host i-* mi-*` pattern in
// ~/.ssh/config); and (2) a direct TCP+SSH-banner probe, as a fallback
// for VPCs with real network access. Separately, each candidate's own
// Security Group egress rules are checked against its VPC's CIDR
// block(s) -- being reachable doesn't guarantee the box can actually
// route traffic to the rest of the VPC once sshuttle is tunneling
// through it.
//
// First run (no saved config): discover, probe, rank, let the user pick,
// re-verify with a real ssh attempt, collect ssh user/key, save.
// Subsequent runs: load the saved target, re-verify, print the
// ready-to-run sshuttle command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bastionctl/internal/awsclient"
	"bastionctl/internal/config"
	"bastionctl/internal/netcidr"
	"bastionctl/internal/probe"
	"bastionctl/internal/sshtest"
	"bastionctl/internal/sshuttlecmd"
	"bastionctl/internal/ui"
)

const probeTimeout = 3 * time.Second
const verifyTimeout = 10 * time.Second

func main() {
	vpcID := flag.String("vpc-id", "", "Target VPC ID (used only when (re)scanning)")
	region := flag.String("region", "", "AWS region override")
	awsProfile := flag.String("aws-profile", "", "AWS shared config profile to use")
	connection := flag.String("connection", "", "Saved bastionctl connection name")
	rescan := flag.Bool("rescan", false, "Force a fresh candidate scan even if a config already exists")
	forget := flag.Bool("forget", false, "Delete the selected saved connection and exit")
	configDirOverride := flag.String("config-dir", "", "Override the bastionctl config directory")
	configPathOverride := flag.String("config-path", "", "Deprecated: infer config directory from this path")
	launch := flag.Bool("launch", false, "Actually execute sshuttle instead of just printing the command")
	start := flag.Bool("start", false, "Start sshuttle as a background daemon and free the terminal")
	stop := flag.Bool("stop", false, "Stop the selected connection's background sshuttle daemon")
	status := flag.Bool("status", false, "Show whether the selected connection is running")
	logs := flag.Bool("logs", false, "Show recent sshuttle daemon logs and follow them live")
	sshPort := flag.Int("ssh-port", 22, "SSH port to probe/use")
	flag.Parse()
	if *connection == "" && flag.NArg() == 1 {
		*connection = flag.Arg(0)
	} else if flag.NArg() > 1 {
		fatal(fmt.Errorf("unexpected arguments: %s", strings.Join(flag.Args()[1:], " ")))
	}
	actionCount := 0
	for _, enabled := range []bool{*launch, *start, *stop, *status, *logs} {
		if enabled {
			actionCount++
		}
	}
	if actionCount > 1 {
		fatal(errors.New("--launch, --start, --stop, --status, and --logs are mutually exclusive"))
	}
	if *logs {
		fmt.Println("Following sshuttle daemon logs. Press Ctrl+C to stop viewing logs.")
		if err := sshuttlecmd.FollowLogs(); err != nil {
			fatal(fmt.Errorf("following sshuttle logs: %w", err))
		}
		return
	}

	configRoot := *configDirOverride
	if configRoot == "" && *configPathOverride != "" {
		configRoot = filepath.Dir(*configPathOverride)
	}
	if configRoot == "" {
		root, err := config.DefaultRoot()
		if err != nil {
			fatal(err)
		}
		configRoot = root
	}

	ctx := context.Background()
	client, err := awsclient.New(ctx, *region, *awsProfile)
	if err != nil {
		fatal(err)
	}
	identity, err := client.GetCallerIdentity(ctx)
	if err != nil {
		fatal(err)
	}
	accountAlias, _ := client.GetAccountAlias(ctx)
	fmt.Printf("AWS account: %s", formatAccount(identity.AccountID, accountAlias))
	if *awsProfile != "" {
		fmt.Printf("  ·  Profile: %s", *awsProfile)
	}
	fmt.Println()

	store := config.NewStore(configRoot)
	if name, migrated, err := store.MigrateLegacy(); err != nil {
		fatal(err)
	} else if migrated {
		fmt.Printf("Migrated legacy config to connection %q (backup: %s).\n", name, filepath.Join(configRoot, "config.yaml.migrated"))
	}
	profiles, err := store.List(identity.AccountID)
	if err != nil {
		fatal(err)
	}

	profileName := *connection
	var cfg *config.Config
	if profileName != "" {
		cfg, err = store.Load(identity.AccountID, profileName)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fatal(err)
		}
	} else if len(profiles) > 0 {
		choices := make([]ui.ConnectionChoice, 0, len(profiles)+1)
		for _, profile := range profiles {
			statusLabel := "[Not running]"
			if pidFile, pathErr := profilePIDFile(store, identity.AccountID, profile.Name); pathErr == nil {
				if pid, running, statusErr := sshuttlecmd.BackgroundStatus(pidFile); statusErr != nil {
					statusLabel = "[Unknown]"
				} else if running {
					statusLabel = fmt.Sprintf("[Running:%d]", pid)
				}
			}
			choices = append(choices, ui.ConnectionChoice{
				Status: statusLabel, Name: profile.Name, Kind: string(profile.Kind),
				Region: profile.Region, Networks: strings.Join(profile.SelectedCIDRs(), ", "),
			})
		}
		if !*forget && !*stop && !*status {
			choices = append(choices, ui.ConnectionChoice{Status: "[New]", Name: "New connection", Networks: "Discover and save another pivot"})
		}
		selected, canceled, err := ui.SelectConnection(choices)
		if err != nil {
			fatal(err)
		}
		if canceled {
			return
		}
		if selected < len(profiles) {
			cfg = profiles[selected]
			profileName = cfg.Name
		}
	}

	if *forget {
		if profileName == "" {
			fatal(errors.New("no saved connection selected"))
		}
		if err := store.Delete(identity.AccountID, profileName); err != nil {
			fatal(fmt.Errorf("removing connection %s: %w", profileName, err))
		}
		fmt.Println("Removed connection:", profileName)
		return
	}
	if *stop {
		if cfg == nil || profileName == "" {
			fatal(errors.New("no saved connection selected"))
		}
		pidFile, err := profilePIDFile(store, identity.AccountID, profileName)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("[Stopping] %s\n", profileName)
		pid, wasRunning, err := sshuttlecmd.StopBackground(pidFile)
		if err != nil {
			fatal(fmt.Errorf("stopping connection %s: %w", profileName, err))
		}
		if !wasRunning {
			fmt.Printf("[Not running] %s\n", profileName)
			return
		}
		fmt.Printf("[Not running] %s (stopped PID %d)\n", profileName, pid)
		return
	}
	if *status {
		if cfg == nil || profileName == "" {
			fatal(errors.New("no saved connection selected"))
		}
		pidFile, err := profilePIDFile(store, identity.AccountID, profileName)
		if err != nil {
			fatal(err)
		}
		pid, running, err := sshuttlecmd.BackgroundStatus(pidFile)
		if err != nil {
			fatal(fmt.Errorf("checking connection %s: %w", profileName, err))
		}
		if running {
			fmt.Printf("[Running] %s (PID %d)\n", profileName, pid)
		} else {
			fmt.Printf("[Not running] %s\n", profileName)
		}
		return
	}

	selectedRegion := *region
	if cfg != nil {
		if *region != "" && *region != cfg.Region {
			fatal(fmt.Errorf("connection %q belongs to region %s; remove --region or rescan into a new connection", profileName, cfg.Region))
		}
		selectedRegion = cfg.Region
	} else if selectedRegion == "" {
		regions, err := client.ListEnabledRegions(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "WARNING: could not list enabled AWS regions; using configured region:", client.Region())
			selectedRegion = client.Region()
		} else if len(regions) == 0 {
			selectedRegion = client.Region()
		} else if len(regions) == 1 {
			selectedRegion = regions[0]
		} else {
			choices := make([]ui.Choice, len(regions))
			for index, regionName := range regions {
				description := "Enabled region"
				if regionName == client.Region() {
					description = "Current AWS default"
				}
				choices[index] = ui.Choice{Title: regionName, Description: description}
			}
			selected, canceled, err := ui.Select("Select an AWS region", choices)
			if err != nil {
				fatal(err)
			}
			if canceled {
				return
			}
			selectedRegion = regions[selected]
		}
	}
	if selectedRegion != client.Region() {
		client, err = awsclient.New(ctx, selectedRegion, *awsProfile)
		if err != nil {
			fatal(err)
		}
		selectedIdentity, err := client.GetCallerIdentity(ctx)
		if err != nil {
			fatal(err)
		}
		if selectedIdentity.AccountID != identity.AccountID {
			fatal(fmt.Errorf("region client resolved account %s instead of %s", selectedIdentity.AccountID, identity.AccountID))
		}
	}
	// The SSM SSH ProxyCommand runs the AWS CLI outside the Go SDK. Keep it on
	// the same region/profile as discovery and the saved connection.
	if err := os.Setenv("AWS_REGION", selectedRegion); err != nil {
		fatal(fmt.Errorf("setting AWS_REGION: %w", err))
	}
	if err := os.Setenv("AWS_DEFAULT_REGION", selectedRegion); err != nil {
		fatal(fmt.Errorf("setting AWS_DEFAULT_REGION: %w", err))
	}
	if *awsProfile != "" {
		if err := os.Setenv("AWS_PROFILE", *awsProfile); err != nil {
			fatal(fmt.Errorf("setting AWS_PROFILE: %w", err))
		}
	}
	fmt.Println("Region:", client.Region())

	if cfg != nil && !*rescan {
		fmt.Printf("Loaded connection %q (method: %s)\n", profileName, cfg.ConnectionMethod)
		ok, detail := verifyTarget(cfg.ConnectionMethod, cfg.Target, cfg.SSHPort)
		if !ok {
			fmt.Printf("WARNING: saved target %s is not reachable right now. Run with --rescan to pick a new one.\n", cfg.Target)
			if detail != "" {
				fmt.Println(detail)
			}
		} else {
			fmt.Println("Reachability check OK.")
			if detail != "" {
				fmt.Println(detail)
			}
		}
	} else {
		if profileName == "" {
			for {
				profileName = ui.PromptString("Connection name", "default")
				if err := config.ValidateProfileName(profileName); err == nil {
					break
				} else {
					fmt.Println(err)
				}
			}
		}
		kind := config.ProfileKindVPC
		if cfg != nil && cfg.Kind != "" {
			kind = cfg.Kind
		} else {
			selected, canceled, err := ui.Select("Connection type", []ui.Choice{
				{Title: "VPC", Description: "Tunnel the pivot VPC CIDRs"},
				{Title: "Dedicated network", Description: "Choose subnet routes and custom CIDRs"},
			})
			if err != nil {
				fatal(err)
			}
			if canceled {
				return
			}
			if selected == 1 {
				kind = config.ProfileKindDedicated
			}
		}

		newCfg, err := runDiscoveryFlow(ctx, client, identity, *vpcID, *sshPort, kind, *awsProfile)
		if err != nil {
			fatal(err)
		}
		if newCfg == nil {
			fmt.Println("No target selected/saved. Exiting.")
			return
		}
		if cfg != nil && cfg.SavedAt != "" {
			newCfg.SavedAt = cfg.SavedAt
		}
		if err := store.Save(identity.AccountID, profileName, newCfg); err != nil {
			fatal(fmt.Errorf("saving config: %w", err))
		}
		fmt.Printf("Saved connection %q.\n", profileName)
		cfg = newCfg
	}
	if len(cfg.SelectedCIDRs()) == 0 {
		fatal(fmt.Errorf("connection %q has no selected networks; run with --rescan", profileName))
	}
	cfg.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	if err := store.Save(identity.AccountID, profileName, cfg); err != nil {
		fatal(fmt.Errorf("updating connection usage: %w", err))
	}
	cfgPath, err := store.ProfilePath(identity.AccountID, profileName)
	if err != nil {
		fatal(err)
	}

	bin, err := sshuttlecmd.FindBinary()
	if err != nil {
		ui.PrintInstallHint()
		return
	}

	args := sshuttlecmd.BuildArgs(cfg)
	fmt.Println("Networks:", strings.Join(cfg.SelectedCIDRs(), ", "))
	if *start {
		pidFile := strings.TrimSuffix(cfgPath, filepath.Ext(cfgPath)) + ".pid"
		if pid, running, statusErr := sshuttlecmd.BackgroundStatus(pidFile); statusErr != nil {
			fatal(fmt.Errorf("checking connection %s: %w", profileName, statusErr))
		} else if running {
			fmt.Printf("[Running] %s (PID %d)\n", profileName, pid)
			return
		}
		fmt.Printf("[Starting] %s\n", profileName)
		pid, err := sshuttlecmd.StartBackground(bin, args, pidFile)
		if err != nil {
			fatal(fmt.Errorf("starting sshuttle in background: %w", err))
		}
		fmt.Printf("[Running] %s (PID %d)\n", profileName, pid)
		fmt.Println("PID file:", pidFile)
		fmt.Printf("Stop it with: ./bastionctl --stop %s\n", profileName)
		fmt.Println("Live logs: ./bastionctl --logs")
	} else if *launch {
		if err := sshuttlecmd.Launch(bin, args); err != nil {
			fatal(fmt.Errorf("sshuttle exited with error: %w", err))
		}
	} else {
		sshuttlecmd.PrintCommand(bin, args)
	}
}

func profilePIDFile(store *config.Store, accountID, profileName string) (string, error) {
	profilePath, err := store.ProfilePath(accountID, profileName)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(profilePath, filepath.Ext(profilePath)) + ".pid", nil
}

func formatAccount(accountID, alias string) string {
	if alias == "" {
		return accountID
	}
	return fmt.Sprintf("%s [%s]", alias, accountID)
}

// verifyTarget re-checks connectivity using the appropriate method.
func verifyTarget(method, target string, port int) (bool, string) {
	if method == probe.MethodSSM {
		return sshtest.TryConnect(target, port, verifyTimeout)
	}
	return probe.ProbeSSH(target, port, verifyTimeout)
}

// runDiscoveryFlow performs the interactive scan -> probe -> select ->
// verify -> collect-credentials sequence. Returns nil (no error) if the
// user aborts without selecting/saving anything.
func runDiscoveryFlow(ctx context.Context, client *awsclient.Client, identity *awsclient.Identity, vpcID string, sshPort int, kind config.ProfileKind, awsProfile string) (*config.Config, error) {
	vpcs, mode, err := client.ListTargetVPCs(ctx, vpcID)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "auto_single":
		fmt.Println("Scanning the only VPC in this region:", vpcs[0].VpcID)
	case "auto_all":
		choices := make([]ui.Choice, len(vpcs))
		for index, vpc := range vpcs {
			name := vpc.Name
			if name == "" {
				name = "unnamed"
			}
			choices[index] = ui.Choice{
				Title:       fmt.Sprintf("%s  %s", name, vpc.VpcID),
				Description: strings.Join(vpc.CIDRBlocks, ", "),
			}
		}
		selected, canceled, err := ui.Select("Select a VPC to scan", choices)
		if err != nil {
			return nil, err
		}
		if canceled {
			return nil, nil
		}
		vpcs = []awsclient.VPC{vpcs[selected]}
		fmt.Printf("Scanning VPC %s (%s).\n", vpcs[0].VpcID, strings.Join(vpcs[0].CIDRBlocks, ", "))
	}

	var candidates []probe.Candidate
	vpcByInstance := map[string]awsclient.VPC{}
	sgIDsByInstance := map[string][]string{}
	sgIDSet := map[string]struct{}{}

	for _, vpc := range vpcs {
		instances, err := client.ListRunningInstances(ctx, vpc.VpcID)
		if err != nil {
			return nil, err
		}
		for _, inst := range instances {
			candidates = append(candidates, probe.Candidate{
				InstanceID: inst.InstanceID,
				Name:       inst.NameTag,
				VpcID:      vpc.VpcID,
				VpcCIDRs:   vpc.CIDRBlocks,
				SubnetID:   inst.SubnetID,
				PublicIP:   inst.PublicIP,
				PrivateIP:  inst.PrivateIP,
			})
			vpcByInstance[inst.InstanceID] = vpc
			sgIDsByInstance[inst.InstanceID] = inst.SecurityGroupIDs
			for _, id := range inst.SecurityGroupIDs {
				sgIDSet[id] = struct{}{}
			}
		}
	}

	if len(candidates) == 0 {
		fmt.Println("No running instances found in scanned VPC(s).")
		return nil, nil
	}

	instanceIDs := make([]string, len(candidates))
	for i, c := range candidates {
		instanceIDs[i] = c.InstanceID
	}

	fmt.Println("Checking AWS Systems Manager (Session Manager) status...")
	ssmOnline, err := client.ListSSMOnlineInstanceIDs(ctx, instanceIDs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING: could not query SSM instance status:", err)
		ssmOnline = map[string]bool{}
	}

	fmt.Printf("Probing %d instance(s) for direct TCP/SSH reachability on port %d (fallback signal)...\n", len(candidates), sshPort)
	probe.ProbeAll(candidates, sshPort, probeTimeout)

	fmt.Println("Checking Security Group egress coverage of each VPC's CIDR block(s)...")
	sgIDs := make([]string, 0, len(sgIDSet))
	for id := range sgIDSet {
		sgIDs = append(sgIDs, id)
	}
	egressBySG, err := client.DescribeSecurityGroupEgress(ctx, sgIDs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING: could not describe security group egress rules:", err)
		egressBySG = map[string][]awsclient.EgressRule{}
	}

	for i := range candidates {
		c := &candidates[i]

		// Reachability: SSM takes priority over the direct-TCP fallback
		// signal already populated by ProbeAll.
		if ssmOnline[c.InstanceID] {
			c.Method = probe.MethodSSM
			c.Reachable = true
			c.Target = c.InstanceID
		}

		// VPC egress coverage, regardless of reachability method.
		vpc := vpcByInstance[c.InstanceID]
		var allCIDRs []string
		for _, sgID := range sgIDsByInstance[c.InstanceID] {
			for _, rule := range egressBySG[sgID] {
				allCIDRs = append(allCIDRs, rule.CIDR)
			}
		}
		ok := true
		var gaps []string
		for _, block := range vpc.CIDRBlocks {
			if !netcidr.CoversAny(allCIDRs, block) {
				ok = false
				gaps = append(gaps, block)
			}
		}
		c.VpcEgressOK = ok
		c.VpcEgressGaps = gaps
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return probe.Rank(candidates[i]) < probe.Rank(candidates[j])
	})

	reachableCount := 0
	for _, c := range candidates {
		if c.Reachable {
			reachableCount++
		}
	}
	if reachableCount == 0 {
		fmt.Println("No reachable candidates found. This VPC may need existing VPN/peering access, " +
			"no instance is SSM-managed/online, and no instance allows inbound SSH from your location.")
		return nil, nil
	}

	idx, canceled, err := ui.SelectCandidate(candidates)
	if err != nil {
		return nil, err
	}
	if canceled {
		return nil, nil
	}
	chosen := candidates[idx]

	if !chosen.Reachable {
		fmt.Println("That candidate was not reachable during the scan. Pick a reachable one instead.")
		return nil, nil
	}

	if !chosen.VpcEgressOK {
		fmt.Printf("WARNING: this instance's Security Group egress does not cover: %s\n", joinOrDash(chosen.VpcEgressGaps))
		fmt.Println("sshuttle may not be able to route to those ranges through this pivot.")
		if !ui.PromptYesNo("Continue with this candidate anyway?", false) {
			return nil, nil
		}
	}

	fmt.Println("Re-verifying connectivity to", chosen.Target, "via", chosen.Method, "...")
	ok, detail := verifyTarget(chosen.Method, chosen.Target, sshPort)
	if !ok {
		fmt.Println("Connection failed on re-check:")
		if detail != "" {
			fmt.Println(detail)
		}
		fmt.Println("Re-run and pick a different candidate.")
		return nil, nil
	}
	fmt.Println("Connected OK.")
	if detail != "" {
		fmt.Println(detail)
	}

	routes, err := profileRoutes(ctx, client, chosen, kind)
	if err != nil {
		return nil, err
	}

	var sshUser, sshKeyPath string
	if chosen.Method == probe.MethodSSM {
		fmt.Println("This target connects via SSM, matching a Host pattern in ~/.ssh/config. " +
			"Leave user/key blank to use that config as-is.")
		sshUser = ui.PromptString("SSH username override (blank = use ~/.ssh/config)", "")
		sshKeyPath = ui.PromptString("SSH key path override (blank = use ~/.ssh/config)", "")
	} else {
		sshUser = ui.PromptString("SSH username", "ec2-user")
		sshKeyPath = ui.PromptString("SSH private key path (blank = use ssh-agent/default identity)", "")
	}

	if !ui.PromptYesNo("Save this connection?", true) {
		return nil, nil
	}

	vpc := vpcByInstance[chosen.InstanceID]
	now := time.Now().UTC().Format(time.RFC3339)
	cfg := &config.Config{
		Kind:             kind,
		AccountID:        identity.AccountID,
		AWSProfile:       awsProfile,
		Region:           client.Region(),
		VpcID:            vpc.VpcID,
		VpcCIDRs:         vpc.CIDRBlocks,
		SubnetID:         chosen.SubnetID,
		InstanceID:       chosen.InstanceID,
		NameTag:          chosen.Name,
		Routes:           routes,
		ConnectionMethod: chosen.Method,
		Target:           chosen.Target,
		SSHUser:          sshUser,
		SSHKeyPath:       sshKeyPath,
		SSHPort:          sshPort,
		SavedAt:          now,
		UpdatedAt:        now,
	}
	return cfg, nil
}

func profileRoutes(ctx context.Context, client *awsclient.Client, chosen probe.Candidate, kind config.ProfileKind) ([]config.NetworkRoute, error) {
	var routes []config.NetworkRoute
	seen := map[string]struct{}{}
	add := func(route config.NetworkRoute) error {
		cidr, err := netcidr.Normalize(route.CIDR)
		if err != nil {
			return err
		}
		if _, exists := seen[cidr]; exists {
			return nil
		}
		seen[cidr] = struct{}{}
		route.CIDR = cidr
		routes = append(routes, route)
		return nil
	}
	for _, cidr := range chosen.VpcCIDRs {
		if err := add(config.NetworkRoute{CIDR: cidr, Source: "pivot_vpc", Selected: true}); err != nil {
			return nil, err
		}
	}
	if kind == config.ProfileKindVPC {
		return routes, nil
	}

	discovered, err := client.GetEffectiveSubnetRoutes(ctx, chosen.VpcID, chosen.SubnetID)
	if err != nil {
		fmt.Fprintln(os.Stderr, ui.Warning("WARNING: route discovery unavailable: "+err.Error()))
	} else {
		for _, route := range discovered {
			if err := add(config.NetworkRoute{
				CIDR: route.CIDR, Source: "subnet_route", TargetType: route.TargetType, TargetID: route.TargetID,
			}); err != nil {
				return nil, err
			}
		}
	}

	for {
		manual := ui.PromptString("Additional CIDRs (comma or space separated, blank = none)", "")
		fields := strings.FieldsFunc(manual, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		normalized := make([]string, 0, len(fields))
		valid := true
		for _, cidr := range fields {
			value, err := netcidr.Normalize(cidr)
			if err != nil {
				fmt.Println(err)
				valid = false
				break
			}
			normalized = append(normalized, value)
		}
		if !valid {
			continue
		}
		for _, cidr := range normalized {
			if err := add(config.NetworkRoute{CIDR: cidr, Source: "manual", Selected: true}); err != nil {
				return nil, err
			}
		}
		break
	}
	selected, canceled, err := ui.SelectRoutes(routes)
	if err != nil {
		return nil, err
	}
	if canceled {
		return nil, errors.New("network selection canceled")
	}
	for _, route := range selected {
		if route.Selected {
			return selected, nil
		}
	}
	return nil, errors.New("select at least one network")
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	out := items[0]
	for _, s := range items[1:] {
		out += ", " + s
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
