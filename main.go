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
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"bastionctl/internal/awsclient"
	"bastionctl/internal/config"
	"bastionctl/internal/dnsresolver"
	"bastionctl/internal/netcidr"
	"bastionctl/internal/probe"
	"bastionctl/internal/sshtest"
	"bastionctl/internal/sshuttlecmd"
	"bastionctl/internal/ui"
)

const probeTimeout = 3 * time.Second
const verifyTimeout = 10 * time.Second

// extractDemoFlag removes a hidden --demo flag from os.Args and reports
// whether it was present. Handled manually so flag.Parse keeps erroring on
// typos while --help stays clean.
func extractDemoFlag() bool {
	demo := false
	kept := make([]string, 0, len(os.Args))
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--demo", "-demo", "--demo=true", "-demo=true":
			demo = true
		case "--demo=false", "-demo=false":
			// Explicit off: just drop it.
		default:
			kept = append(kept, arg)
		}
	}
	if demo {
		os.Args = append([]string{os.Args[0]}, kept...)
	}
	return demo
}

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
	dnsMode := flag.String("dns", "", "DNS tunneling mode for the selected connection: off, all, or split (persistent override)")
	dnsDomains := flag.String("dns-domain", "", "Split-DNS domain suffixes, comma/space separated (persistent override)")
	dnsResolver := flag.String("dns-resolver", "", "DNS server IPs to intercept, comma/space separated (persistent override)")
	dnsToNS := flag.String("dns-to-ns", "", "Remote forward target IP[:port] for captured DNS, sshuttle --to-ns (persistent override)")
	manageDNS := flag.Bool("manage-dns", false, "Install/remove the split-DNS OS resolver config automatically (sudo is used only for those system changes)")
	dnsSetup := flag.Bool("dns-setup", false, "Install the selected connection's split-DNS resolver config and exit")
	dnsTeardown := flag.Bool("dns-teardown", false, "Remove the selected connection's installed split-DNS resolver config and exit")

	// Hidden mode: --demo runs the browser against simulated state (no AWS,
	// no config writes) for demos and screen recordings. It is peeled off
	// os.Args before flag.Parse so it never shows up in --help.
	demoRequested := extractDemoFlag()

	flag.Parse()

	if demoRequested {
		if !ui.Interactive() {
			fmt.Println("Demo mode needs a real terminal; run bastionctl --demo interactively.")
			return
		}
		runDemoApp()
		return
	}

	dnsModeOverride := ""
	if *dnsMode != "" {
		mode, err := config.ParseDNSMode(*dnsMode)
		if err != nil {
			fatal(err)
		}
		dnsModeOverride = string(mode)
	}
	dnsFlagsSet := dnsModeOverride != "" || *dnsDomains != "" || *dnsResolver != "" || *dnsToNS != ""

	actionCount := 0
	for _, enabled := range []bool{*launch, *start, *stop, *status, *logs, *dnsSetup, *dnsTeardown} {
		if enabled {
			actionCount++
		}
	}
	if actionCount > 1 {
		fatal(errors.New("--launch, --start, --stop, --status, --logs, --dns-setup, and --dns-teardown are mutually exclusive"))
	}
	if dnsFlagsSet && (*forget || *stop || *status || *logs || *dnsTeardown) {
		fatal(errors.New("--dns settings cannot be combined with --forget, --stop, --status, --logs, or --dns-teardown"))
	}
	if *connection == "" && flag.NArg() == 1 {
		*connection = flag.Arg(0)
	} else if flag.NArg() > 1 {
		fatal(fmt.Errorf("unexpected arguments: %s", strings.Join(flag.Args()[1:], " ")))
	}
	if *logs {
		followLogsAction(false)
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
	identity, identityErr := client.GetCallerIdentity(ctx)
	offline := identityErr != nil
	var accountAlias string
	if offline {
		fmt.Fprintln(os.Stderr, "WARNING: could not reach AWS STS:", identityErr)
		fmt.Fprintln(os.Stderr, "Continuing offline: saved connections can be browsed and managed from the cache.")
	} else {
		accountAlias, _ = client.GetAccountAlias(ctx)
		fmt.Printf("AWS account: %s", formatAccount(identity.AccountID, accountAlias))
		if *awsProfile != "" {
			fmt.Printf("  ·  Profile: %s", *awsProfile)
		}
		fmt.Println()
	}

	store := config.NewStore(configRoot)
	if name, migrated, err := store.MigrateLegacy(); err != nil {
		fatal(err)
	} else if migrated {
		fmt.Printf("Migrated legacy config to connection %q (backup: %s).\n", name, filepath.Join(configRoot, "config.yaml.migrated"))
	}

	env := browserEnv{
		ctx:          ctx,
		client:       client,
		identity:     identity,
		identityErr:  identityErr,
		accountAlias: accountAlias,
		store:        store,
		configRoot:   configRoot,
		awsProfile:   *awsProfile,
		manageDNS:    *manageDNS,
		sshPort:      *sshPort,
		regionFlag:   *region,
		vpcIDFlag:    *vpcID,
		dnsModeFlag:  dnsModeOverride,
		dnsDomains:   *dnsDomains,
		dnsResolver:  *dnsResolver,
		dnsToNS:      *dnsToNS,
	}

	profiles, err := listAllConnections(store, configRoot)
	if err != nil {
		fatal(err)
	}

	profileName := *connection
	var cfg *config.Config
	if profileName != "" {
		for _, profile := range profiles {
			match := profile.Name == profileName
			if match && (offline || profile.AccountID == identity.AccountID) {
				cfg = profile
				break
			}
		}
	}

	// Interactive runs without action flags open the k9s-style browser;
	// every scripted invocation keeps the classic single-shot behavior.
	scripted := *start || *launch || *stop || *status || *forget || *rescan || *dnsSetup || *dnsTeardown || dnsFlagsSet
	if !scripted && ui.Interactive() {
		if profileName != "" && cfg == nil {
			fatal(fmt.Errorf("connection %q not found in the cache", profileName))
		}
		env.openConnection = profileName
		env.openCfg = cfg
		runBrowser(&env)
		return
	}

	if offline {
		if *rescan {
			fatal(fmt.Errorf("rescan needs AWS connectivity: %w", identityErr))
		}
		if cfg == nil {
			if len(profiles) > 0 {
				fatal(fmt.Errorf("offline: connection %q is not in the cache; cached: %s", profileName, connectionNames(profiles)))
			}
			fatal(fmt.Errorf("offline and no cached connections; start bastionctl again once AWS is reachable"))
		}
		// Account-scoped helpers work from the connection's own account id
		// while STS is unreachable.
		identity = &awsclient.Identity{AccountID: cfg.AccountID}
		if client.Region() != cfg.Region {
			regionClient, err := awsclient.New(ctx, cfg.Region, *awsProfile)
			if err == nil {
				client = regionClient
			}
		}
	}

	// Non-interactive runs without an explicit connection keep the classic
	// numbered list (browser replaced its TTY variant).
	if cfg == nil && profileName == "" && len(profiles) > 0 {
		var candidates []*config.Config
		for _, profile := range profiles {
			if identity != nil && profile.AccountID == identity.AccountID {
				candidates = append(candidates, profile)
			}
		}
		if len(candidates) > 0 {
			fmt.Println("Configured connections")
			for index, profile := range candidates {
				fmt.Printf("  %d. %-20s %-10s %s\n", index+1, profile.Name, profile.Kind, profile.Region)
			}
			choice := ui.PromptSelect(len(candidates), 1)
			cfg = candidates[choice-1]
			profileName = cfg.Name
		}
	}
	_ = profiles

	if *forget {
		actionForget(store, identity.AccountID, profileName)
		return
	}
	if *stop {
		actionStop(store, identity.AccountID, profileName)
		return
	}
	if *status {
		actionStatus(store, identity.AccountID, profileName)
		return
	}
	if *dnsTeardown {
		actionDNSTeardown(store, identity.AccountID, profileName)
		return
	}
	if *dnsSetup {
		actionDNSSetup(store, identity.AccountID, profileName, cfg)
		return
	}

	// Persistent DNS overrides for an already-saved connection.
	if dnsFlagsSet && cfg != nil && profileName != "" {
		if err := applyDNSFlags(cfg, dnsModeOverride, *dnsDomains, *dnsResolver, *dnsToNS); err != nil {
			fatal(err)
		}
		if err := store.Save(identity.AccountID, profileName, cfg); err != nil {
			fatal(fmt.Errorf("saving connection DNS settings: %w", err))
		}
		fmt.Printf("Updated DNS settings for connection %q (mode: %s).\n", profileName, cfg.DNS.EffectiveMode())
		reconcileDNS(store, identity.AccountID, profileName, cfg, *manageDNS)
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
		if cfg.DNS != nil && cfg.DNS.EffectiveMode() != config.DNSModeOff {
			fmt.Println("DNS:", cfg.DNSSummary())
		}
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

		var prevDNS *config.DNSConfig
		if cfg != nil {
			prevDNS = cfg.DNS
		}
		dnsPreset := dnsPresetFromFlags(dnsModeOverride, *dnsDomains, *dnsResolver, *dnsToNS)

		newCfg, err := runDiscoveryFlow(ctx, client, identity, *vpcID, *sshPort, kind, *awsProfile, prevDNS, dnsPreset, "")
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
		reconcileDNS(store, identity.AccountID, profileName, cfg, *manageDNS)
	}
	if len(cfg.SelectedCIDRs()) == 0 {
		fatal(fmt.Errorf("connection %q has no selected networks; run with --rescan", profileName))
	}
	cfg.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	if err := store.Save(identity.AccountID, profileName, cfg); err != nil {
		fatal(fmt.Errorf("updating connection usage: %w", err))
	}

	bin, err := sshuttlecmd.FindBinary()
	if err != nil {
		ui.PrintInstallHint()
		return
	}

	fmt.Println("Networks:", strings.Join(cfg.SelectedCIDRs(), ", "))

	connDNSMode := cfg.DNS.EffectiveMode()
	manageDNSNow := (cfg.DNS != nil && cfg.DNS.ManageResolver) || *manageDNS
	if connDNSMode != config.DNSModeOff {
		fmt.Println("DNS:", cfg.DNSSummary())
	}

	// Scripted paths only: interactive no-flag runs already went through the
	// browser. Every remaining flag maps to one single-shot action.
	connectionProfile := effectiveAWSProfile(cfg, *awsProfile)
	if *start {
		withAWSProfile(connectionProfile, func() {
			actionStartTunnel(store, identity.AccountID, profileName, cfg, bin, manageDNSNow)
		})
	} else if *launch {
		withAWSProfile(connectionProfile, func() {
			actionLaunchTunnel(store, identity.AccountID, profileName, cfg, bin, manageDNSNow)
		})
	} else {
		actionPrintCommand(cfg, bin)
	}
}

func profilePIDFile(store *config.Store, accountID, profileName string) (string, error) {
	profilePath, err := store.ProfilePath(accountID, profileName)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(profilePath, filepath.Ext(profilePath)) + ".pid", nil
}

// profileDNSStateFile is the manifest recording what split-DNS resolver
// changes bastionctl applied for a connection (see internal/dnsresolver).
func profileDNSStateFile(store *config.Store, accountID, profileName string) (string, error) {
	profilePath, err := store.ProfilePath(accountID, profileName)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(profilePath, filepath.Ext(profilePath)) + ".dns-state.yaml", nil
}

// splitFields splits comma/space/tab separated input.
func splitFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}

// dnsPresetFromFlags builds the DNS prompt prefill for a fresh discovery
// from any --dns* command-line flags. nil means "no preset".
func dnsPresetFromFlags(mode, domainsFlag, resolverFlag, toNSFlag string) *config.DNSConfig {
	if mode == "" && domainsFlag == "" && resolverFlag == "" && toNSFlag == "" {
		return nil
	}
	preset := &config.DNSConfig{}
	if mode != "" {
		preset.Mode = config.DNSMode(mode)
	}
	if domainsFlag != "" {
		preset.Domains = splitFields(domainsFlag)
	}
	if resolverFlag != "" {
		preset.Resolver = splitFields(resolverFlag)
	}
	if toNSFlag != "" {
		preset.ToNS = toNSFlag
	}
	return preset
}

// applyDNSFlags applies persistent --dns* overrides to a loaded connection
// and validates the result.
func applyDNSFlags(cfg *config.Config, mode, domainsFlag, resolverFlag, toNSFlag string) error {
	if cfg.DNS == nil {
		cfg.DNS = &config.DNSConfig{}
	}
	if mode != "" {
		cfg.DNS.Mode = config.DNSMode(mode)
	}
	if domainsFlag != "" {
		cfg.DNS.Domains = splitFields(domainsFlag)
	}
	if resolverFlag != "" {
		cfg.DNS.Resolver = splitFields(resolverFlag)
	}
	if toNSFlag != "" {
		cfg.DNS.ToNS = toNSFlag
	}
	return cfg.DNS.Normalize()
}

// detectVPCResolver returns the resolver IP derived from the VPC's primary
// IPv4 CIDR ("+2" AWS button rule), or "" when no CIDR allows the derivation.
func detectVPCResolver(vpcCIDRs []string) string {
	for _, cidr := range vpcCIDRs {
		if addr, err := netcidr.SecondIPv4Host(cidr); err == nil {
			return addr.String()
		}
	}
	return ""
}

// teardownDNSState removes the recorded split-DNS resolver config (if any)
// and prints a summary. reportMissing controls the informational line when
// nothing was installed.
func teardownDNSState(store *config.Store, accountID, profileName string, reportMissing bool) {
	statePath, err := profileDNSStateFile(store, accountID, profileName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING:", err)
		return
	}
	state, err := dnsresolver.LoadState(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING:", err)
		return
	}
	if state == nil {
		if reportMissing {
			fmt.Println("No split-DNS resolver config installed.")
		}
		return
	}
	warnings, rerr := dnsresolver.Remove(state)
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, "WARNING: "+warning)
	}
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not remove split-DNS resolver config: %v\nClean up later with: bastionctl --dns-teardown %s\n", rerr, profileName)
		return
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "WARNING: removing split-DNS state file:", err)
	}
	fmt.Println("Removed split-DNS resolver config.")
}

// undoDNSInstall rolls back an install made for a run that immediately
// failed (e.g. sshuttle did not start).
func undoDNSInstall(state *dnsresolver.State, statePath, profileName string) {
	if state == nil {
		return
	}
	warnings, rerr := dnsresolver.Remove(state)
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, "WARNING: "+warning)
	}
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not roll back split-DNS resolver config: %v; clean up with: bastionctl --dns-teardown %s\n", rerr, profileName)
		return
	}
	_ = os.Remove(statePath)
}

// printDNSState adds the resolver-config status line to --status output.
func printDNSState(store *config.Store, accountID, profileName string, tunnelRunning bool) {
	statePath, err := profileDNSStateFile(store, accountID, profileName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING:", err)
		return
	}
	state, err := dnsresolver.LoadState(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING:", err)
		return
	}
	if state == nil {
		return
	}
	fmt.Println("DNS:", state.Describe())
	if !tunnelRunning {
		fmt.Printf("WARNING: split-DNS resolver config is installed while the tunnel is down; clean up with: bastionctl --dns-teardown %s\n", profileName)
	}
}

// reconcileDNS brings an already-installed resolver configuration in line
// with freshly changed connection settings: a no-op when nothing was ever
// installed, reinstall/reconcile when split DNS managed, teardown when DNS
// handling was turned off (or dropped below managed split).
func reconcileDNS(store *config.Store, accountID, profileName string, cfg *config.Config, manageOverride bool) {
	if cfg == nil {
		return
	}
	statePath, err := profileDNSStateFile(store, accountID, profileName)
	if err != nil {
		return
	}
	installed, err := dnsresolver.LoadState(statePath)
	if err != nil || installed == nil {
		return
	}
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit && (cfg.DNS.ManageResolver || manageOverride) {
		state, err := dnsresolver.Install(cfg, statePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "WARNING: updating split-DNS resolver config:", err)
			return
		}
		fmt.Println("Updated split-DNS resolver config.", state.Describe())
		return
	}
	teardownDNSState(store, accountID, profileName, false)
}

// promptDNS collects the connection's DNS settings interactively.
//
// preset carries any --dns* CLI flags for a brand-new connection; fields set
// there skip their prompt and are used verbatim. Cancelled menus abort the
// whole discovery by returning (nil, nil), matching how the other
// interactive menus behave.
func promptDNS(prev, preset *config.DNSConfig, defaultResolver string, selectedCIDRs []string) (*config.DNSConfig, error) {
	var mode config.DNSMode
	if preset != nil && preset.Mode != "" {
		mode = preset.Mode
		fmt.Printf("DNS handling (from --dns): %s\n", mode)
	} else {
		selected, canceled, err := ui.Select("DNS handling", []ui.Choice{
			{Title: "Off", Description: "No DNS changes; tunnel routes subnets only"},
			{Title: "All", Description: "Resolve all DNS on the pivot (sshuttle --dns)"},
			{Title: "Split", Description: "Resolve only chosen domains on the pivot (split DNS)"},
		})
		if err != nil {
			return nil, err
		}
		if canceled {
			return nil, nil
		}
		switch selected {
		case 1:
			mode = config.DNSModeAll
		case 2:
			mode = config.DNSModeSplit
		}
	}

	dns := &config.DNSConfig{Mode: mode}

	if mode == config.DNSModeOff {
		return dns, nil
	}

	if mode == config.DNSModeAll {
		dns.ToNS = askForwardPreset(prev, preset)
		if dns.ToNS == "" {
			dns.ToNS = defaultResolver
		}
		if err := dns.Normalize(); err != nil {
			return nil, err
		}
		return dns, nil
	}

	// mode == split.
	var domains []string
	if preset != nil && len(preset.Domains) > 0 {
		presetDomains, err := config.NormalizeDomains(preset.Domains)
		if err != nil {
			return nil, err
		}
		domains = presetDomains
	} else {
		def := ""
		if prev != nil && len(prev.Domains) > 0 {
			def = strings.Join(prev.Domains, ", ")
		}
		for {
			input := ui.PromptString("Split-DNS domain suffixes (comma/space separated; empty quits)", def)
			fields := splitFields(input)
			if len(fields) == 0 {
				return nil, nil
			}
			normalized, err := config.NormalizeDomains(fields)
			if err != nil {
				fmt.Println(err)
				continue
			}
			domains = normalized
			break
		}
	}
	dns.Domains = domains

	var resolver []string
	if preset != nil && len(preset.Resolver) > 0 {
		presetResolver, err := config.NormalizeResolverAddrs(preset.Resolver)
		if err != nil {
			return nil, err
		}
		resolver = presetResolver
		fmt.Printf("Split-DNS resolver (from --dns-resolver): %s\n", strings.Join(resolver, ", "))
	} else {
		def := defaultResolver
		if prev != nil && len(prev.Resolver) > 0 {
			def = strings.Join(prev.Resolver, ", ")
		}
		if def == "" {
			def = "10.0.0.2"
		}
		for {
			input := ui.PromptString("Split-DNS resolver IP(s) to intercept (comma/space separated)", def)
			fields := splitFields(input)
			normalized, err := config.NormalizeResolverAddrs(fields)
			if err != nil {
				fmt.Println(err)
				continue
			}
			resolver = normalized
			break
		}
	}
	dns.Resolver = resolver

	dns.ToNS = askForwardPreset(prev, preset)
	dns.ManageResolver = ui.PromptYesNo("Install split-DNS resolver config automatically (sudo for OS changes)?", prev != nil && prev.ManageResolver)

	if err := dns.Normalize(); err != nil {
		return nil, err
	}

	for _, ip := range dns.Resolver {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		if !netcidr.Contains(selectedCIDRs, addr) {
			fmt.Printf("NOTE: resolver %s is not inside the selected routes; queries sent there are still captured by sshuttle, but the pivot must be able to reach it.\n", ip)
			break
		}
	}
	return dns, nil
}

// askForwardPreset resolves the --to-ns value for the discovery prompt.
func askForwardPreset(prev, preset *config.DNSConfig) string {
	if preset != nil && preset.ToNS != "" {
		return strings.TrimSpace(preset.ToNS)
	}
	def := ""
	if prev != nil {
		def = prev.ToNS
	}
	return strings.TrimSpace(ui.PromptString("Forward target for captured DNS as IP[:port] (blank = pivot's resolver)", def))
}

// ---------------------------------------------------------------------------
// Single-shot actions
//
// Each action maps 1:1 to what a CLI flag used to do inline; the manage menu
// calls the same functions so flag-driven and menu-driven behavior is
// guaranteed to stay in sync.
// ---------------------------------------------------------------------------

func followLogsAction(inMenu bool) {
	fmt.Println("Following sshuttle daemon logs. Press Ctrl+C to stop viewing logs.")
	// The log streamer runs as a child in our process group, so Ctrl+C
	// delivers SIGINT to both it and us. Intercept the signal on our side so
	// the streamer exits cleanly but bastionctl stays alive and returns to
	// wherever it was called from (the manage menu).
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	err := sshuttlecmd.FollowLogs()
	if err != nil && !inMenu {
		fatal(fmt.Errorf("following sshuttle logs: %w", err))
	}
	if err != nil {
		// In the menu a stopped stream is a normal exit path, not a crash.
		fmt.Println("Log stream ended.")
	}
}

func actionForget(store *config.Store, accountID, profileName string) {
	if profileName == "" {
		fatal(errors.New("no saved connection selected"))
	}
	teardownDNSState(store, accountID, profileName, false)
	if err := store.Delete(accountID, profileName); err != nil {
		fatal(fmt.Errorf("removing connection %s: %w", profileName, err))
	}
	fmt.Println("Removed connection:", profileName)
}

func actionStop(store *config.Store, accountID, profileName string) {
	if profileName == "" {
		fatal(errors.New("no saved connection selected"))
	}
	pidFile, err := profilePIDFile(store, accountID, profileName)
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
	} else {
		fmt.Printf("[Not running] %s (stopped PID %d)\n", profileName, pid)
	}
	teardownDNSState(store, accountID, profileName, false)
}

func actionStatus(store *config.Store, accountID, profileName string) {
	if profileName == "" {
		fatal(errors.New("no saved connection selected"))
	}
	pidFile, err := profilePIDFile(store, accountID, profileName)
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
	printDNSState(store, accountID, profileName, running)
}

func actionDNSTeardown(store *config.Store, accountID, profileName string) {
	if profileName == "" {
		fatal(errors.New("no saved connection selected"))
	}
	teardownDNSState(store, accountID, profileName, true)
}

func actionDNSSetup(store *config.Store, accountID, profileName string, cfg *config.Config) {
	if cfg == nil || profileName == "" {
		fatal(errors.New("no saved connection selected"))
	}
	if cfg.DNS == nil || cfg.DNS.EffectiveMode() != config.DNSModeSplit {
		fatal(errors.New("DNS resolver install requires a connection in split mode (set it with --dns split --dns-domain ... first)"))
	}
	statePath, err := profileDNSStateFile(store, accountID, profileName)
	if err != nil {
		fatal(err)
	}
	state, err := dnsresolver.Install(cfg, statePath)
	if err != nil {
		fatal(fmt.Errorf("installing split-DNS resolver config: %w", err))
	}
	fmt.Println("Split-DNS resolver config installed.", state.Describe())
}

func actionPrintCommand(cfg *config.Config, bin string) {
	sshuttlecmd.PrintCommand(bin, sshuttlecmd.BuildArgs(cfg))
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit {
		fmt.Println("\nSplit-DNS resolver setup (apply once, before starting the tunnel; or rerun with --manage-dns / --dns-setup to apply automatically):")
		fmt.Print(dnsresolver.SetupInstructions(cfg))
		fmt.Println("Teardown when done:")
		fmt.Print(dnsresolver.TeardownInstructions(cfg))
	}
}

func actionStartTunnel(store *config.Store, accountID, profileName string, cfg *config.Config, bin string, manageDNSOverride bool) {
	pidFile, err := profilePIDFile(store, accountID, profileName)
	if err != nil {
		fatal(err)
	}
	statePath, err := profileDNSStateFile(store, accountID, profileName)
	if err != nil {
		fatal(err)
	}
	if pid, running, statusErr := sshuttlecmd.BackgroundStatus(pidFile); statusErr != nil {
		fatal(fmt.Errorf("checking connection %s: %w", profileName, statusErr))
	} else if running {
		fmt.Printf("[Running] %s (PID %d)\n", profileName, pid)
		return
	}
	fmt.Printf("[Starting] %s\n", profileName)
	var dnsState *dnsresolver.State
	splitDNS := cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit
	if splitDNS && manageDNSOverride {
		dnsState, err = dnsresolver.Install(cfg, statePath)
		if err != nil {
			fatal(fmt.Errorf("installing split-DNS resolver config: %w", err))
		}
		fmt.Println("Split-DNS resolver config installed.", dnsState.Describe())
	} else if splitDNS {
		fmt.Println("\nManual split-DNS resolver setup (rerun with --manage-dns, or use --dns-setup):")
		fmt.Print(dnsresolver.SetupInstructions(cfg))
		fmt.Println("NOTE: split DNS will not resolve until the resolver config above is applied.")
	}
	pid, err := sshuttlecmd.StartBackground(bin, sshuttlecmd.BuildArgs(cfg), pidFile)
	if err != nil {
		undoDNSInstall(dnsState, statePath, profileName)
		fatal(fmt.Errorf("starting sshuttle in background: %w", err))
	}
	fmt.Printf("[Running] %s (PID %d)\n", profileName, pid)
	fmt.Println("PID file:", pidFile)
	fmt.Printf("Stop it with: bastionctl --stop %s\n", profileName)
	fmt.Println("Live logs: bastionctl --logs")
}

func actionLaunchTunnel(store *config.Store, accountID, profileName string, cfg *config.Config, bin string, manageDNSOverride bool) {
	statePath, err := profileDNSStateFile(store, accountID, profileName)
	if err != nil {
		fatal(err)
	}
	var dnsState *dnsresolver.State
	splitDNS := cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit
	if splitDNS && manageDNSOverride {
		dnsState, err = dnsresolver.Install(cfg, statePath)
		if err != nil {
			fatal(fmt.Errorf("installing split-DNS resolver config: %w", err))
		}
		fmt.Println("Split-DNS resolver config installed.", dnsState.Describe())
	} else if splitDNS {
		fmt.Println("\nManual split-DNS resolver setup (rerun with --manage-dns, or use --dns-setup):")
		fmt.Print(dnsresolver.SetupInstructions(cfg))
		fmt.Println("NOTE: split DNS will not resolve until the resolver config above is applied.")
	}
	if dnsState != nil {
		// Catch SIGINT/SIGTERM so the cleanup below runs when sshuttle (in
		// the same process group) exits on Ctrl+C; plain Go default behavior
		// would kill us before the resolver config could be removed.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
	}
	launchErr := sshuttlecmd.Launch(bin, sshuttlecmd.BuildArgs(cfg))
	undoDNSInstall(dnsState, statePath, profileName)
	if launchErr != nil {
		fatal(fmt.Errorf("sshuttle exited with error: %w", launchErr))
	}
}

// ---------------------------------------------------------------------------
// k9s-style browser — what a bare interactive `./bastionctl` opens.
//
// runBrowser loops: gather state, run the full-screen UI, execute the single
// Outcome it reports on the real terminal (sudo prompts, log streaming and
// foreground tunnels all need the plain screen), then re-enter the UI.
// ---------------------------------------------------------------------------

type browserEnv struct {
	ctx            context.Context
	client         *awsclient.Client
	identity       *awsclient.Identity
	identityErr    error // non-nil ⇒ offline: browse/maintain cache only
	accountAlias   string
	store          *config.Store
	configRoot     string
	awsProfile     string
	manageDNS      bool
	sshPort        int
	regionFlag     string
	vpcIDFlag      string
	dnsModeFlag    string
	dnsDomains     string
	dnsResolver    string
	dnsToNS        string
	openConnection string
	openCfg        *config.Config
}

type browserFailure struct{ err error }

var browserInteractive bool

func runBrowser(env *browserEnv) {
	browserInteractive = true
	defer func() { browserInteractive = false }()
	bin, err := sshuttlecmd.FindBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "WARNING: sshuttle not found on PATH;", err)
		bin = ""
	}
	awsProfiles, profileErr := awsclient.ListProfiles()
	if profileErr != nil {
		awsProfiles = []string{"default"}
	}
	awsRegions := browserAWSRegions(env)

	level := 0
	profileName := "" // the connection shown at level 1
	homeSel := ""     // highlighted row when back at home
	pendingError := ""
	if profileErr != nil {
		pendingError = "listing AWS profiles: " + profileErr.Error()
	}
	if env.openCfg != nil {
		level = 1
		profileName = env.openConnection
	}

	for {
		// Refresh everything on every entry: actions above change state.
		configSnapshot, _ := listAllConnections(env.store, env.configRoot)
		summaries := gatherSummaries(env)
		highlight := 0
		if homeSel != "" {
			for index, summary := range summaries {
				if summary.Name == homeSel {
					highlight = index
					break
				}
			}
			if highlight == 0 && (len(summaries) == 0 || summaries[0].Name != homeSel) {
				// The selection was forgotten/removed upstream.
				homeSel = ""
			}
		}
		if profileName != "" && !containsConnection(summaries, profileName) {
			// The open connection was forgotten/removed upstream: fall home.
			level = 0
			profileName = ""
		}

		var detail *ui.ConnDetail
		if level == 1 {
			detail = buildDetail(env, profileName, bin)
			if detail == nil {
				level = 0
				profileName = ""
			}
		}

		account := ""
		if env.identityErr == nil {
			account = formatAccount(env.identity.AccountID, env.accountAlias)
		} else {
			account = "(offline)"
		}
		outcome, err := ui.RunApp(ui.RunOptions{
			Account:        account,
			Profile:        env.awsProfile,
			AWSProfiles:    awsProfiles,
			AWSRegions:     awsRegions,
			Error:          pendingError,
			Offline:        env.identityErr != nil,
			Level:          level,
			Highlight:      highlight,
			Summaries:      summaries,
			Detail:         detail,
			ConfigSnapshot: configSnapshot,
			RefreshConfigs: func() ([]*config.Config, error) {
				return listAllConnections(env.store, env.configRoot)
			},
			RefreshStatus: func() []ui.ConnSummary {
				if _, err := listAllConnections(env.store, env.configRoot); err != nil {
					return nil
				}
				return gatherSummaries(env)
			},
			LoadDetail: func(name string) (*ui.ConnDetail, error) {
				detail := buildDetail(env, name, bin)
				if detail == nil {
					return nil, fmt.Errorf("connection %q is no longer available", name)
				}
				return detail, nil
			},
			Actions: map[string]ui.ActionHandler{
				ui.ActStart: func(name string) (ui.ActionResult, error) {
					return browserStartResult(env, bin, name)
				},
				ui.ActStop: func(name string) (ui.ActionResult, error) {
					return browserStopResult(env, name)
				},
				ui.ActForget: func(name string) (ui.ActionResult, error) {
					return browserForgetResult(env, name)
				},
				ui.ActDNSSetup: func(name string) (ui.ActionResult, error) {
					return browserDNSSetupResult(env, name)
				},
				ui.ActDNSTeardown: func(name string) (ui.ActionResult, error) {
					return browserDNSTeardownResult(env, name)
				},
				ui.ActStatus: func(name string) (ui.ActionResult, error) {
					return browserActionResult(env, bin, ui.ActStatus, name)
				},
				ui.ActPrint: func(name string) (ui.ActionResult, error) {
					return browserActionResult(env, bin, ui.ActPrint, name)
				},
				ui.ActAWSCheck: func(name string) (ui.ActionResult, error) {
					profiles, err := listAllConnections(env.store, env.configRoot)
					if err != nil {
						return ui.ActionResult{}, err
					}
					for _, cfg := range profiles {
						if cfg.Name == name {
							profile := effectiveAWSProfile(cfg, env.awsProfile)
							if profile == "" {
								profile = "default"
							}
							return checkConnectionAWSProfile(env.ctx, cfg, profile)
						}
					}
					return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", name)
				},
			},
			StartLogs: func(string) (*ui.LogSession, error) {
				session, err := sshuttlecmd.StartLogStream()
				if err != nil {
					return nil, err
				}
				return &ui.LogSession{Lines: session.Lines, Done: session.Done, Cancel: session.Cancel}, nil
			},
			StartLaunch: func(name string) (*ui.LogSession, error) {
				return browserLaunchStream(env, bin, name)
			},
			LoadConfig: func(name string) (*config.Config, error) {
				profiles, err := listAllConnections(env.store, env.configRoot)
				if err != nil {
					return nil, err
				}
				for _, profile := range profiles {
					if profile.Name == name {
						return profile, nil
					}
				}
				return nil, fmt.Errorf("connection %q is no longer available", name)
			},
			SaveConfig: func(name string, cfg *config.Config) error {
				accountID := cfg.AccountID
				if accountID == "" {
					accountID = env.identity.AccountID
				}
				newName := cfg.Name
				cfg.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				if err := env.store.Save(accountID, newName, cfg); err != nil {
					return err
				}
				if newName != name {
					if err := env.store.Delete(accountID, name); err != nil {
						return fmt.Errorf("renaming connection: %w", err)
					}
					if _, err := removeBrowserDNSState(env.store, accountID, name); err != nil {
						return fmt.Errorf("connection saved, but old DNS state could not be removed: %w", err)
					}
				}
				if err := reconcileDNSInBrowser(env.store, accountID, newName, cfg, env.manageDNS); err != nil {
					return fmt.Errorf("connection saved, but DNS reconciliation failed: %w", err)
				}
				return nil
			},
			CheckAWSProfile: func(cfg *config.Config, profile string) (ui.ActionResult, error) {
				return checkConnectionAWSProfile(env.ctx, cfg, profile)
			},
			RefreshConnection: func(name string) (ui.ConnSummary, *ui.ConnDetail, error) {
				return browserConnectionSnapshot(env, bin, name)
			},
			PrepareRescan: func(name string) ([]ui.RescanCandidate, error) {
				return prepareRescanCandidates(env, name)
			},
			SaveRescan: func(name string, candidate ui.RescanCandidate) (*config.Config, error) {
				return saveRescanCandidate(env, name, candidate)
			},
			StartNew: func() (ui.NewConnectionStart, error) {
				return newConnectionStartData(env)
			},
			ScanNew: func(region, kind string) ([]ui.RescanCandidate, error) {
				return scanNewCandidates(env, region)
			},
			PrepareNewRoutes: func(region, kind string, candidate ui.RescanCandidate) ([]config.NetworkRoute, error) {
				return prepareNewRoutes(env, region, config.ProfileKind(kind), candidate)
			},
			SaveNewConfig: func(cfg *config.Config) error {
				cfg.SavedAt = time.Now().UTC().Format(time.RFC3339)
				cfg.UpdatedAt = cfg.SavedAt
				profiles, err := listAllConnections(env.store, env.configRoot)
				if err != nil {
					return err
				}
				for _, profile := range profiles {
					if profile.AccountID == cfg.AccountID && profile.Name == cfg.Name {
						return fmt.Errorf("connection %q already exists", cfg.Name)
					}
				}
				if err := env.store.Save(cfg.AccountID, cfg.Name, cfg); err != nil {
					return err
				}
				if err := reconcileDNSInBrowser(env.store, cfg.AccountID, cfg.Name, cfg, env.manageDNS); err != nil {
					return fmt.Errorf("connection saved, but DNS reconciliation failed: %w", err)
				}
				return nil
			},
		})
		pendingError = ""
		if err != nil {
			pendingError = "UI action failed: " + err.Error()
			continue
		}
		if outcome.Quit {
			return
		}
		if outcome.Action == "" {
			// Back to home; keep the row the user had open highlighted.
			level = 0
			profileName = ""
			if outcome.Connection != "" {
				homeSel = outcome.Connection
			}
			continue
		}
		if outcome.Action == ui.ActOpen {
			level = 1
			profileName = outcome.Connection
			homeSel = outcome.Connection
			continue
		}
		// Interactive actions must be handled by RunApp; never hand the
		// alternate screen back to a legacy terminal action.
		pendingError = fmt.Sprintf("Action %q has no in-app handler", outcome.Action)
		if outcome.View == ui.ViewConnection {
			level = 1
			profileName = outcome.Connection
		} else {
			level = 0
			profileName = ""
		}
		homeSel = outcome.Connection
	}
}

func browserAWSRegions(env *browserEnv) []string {
	seen := make(map[string]struct{})
	var regions []string
	add := func(region string) {
		if region == "" {
			return
		}
		if _, ok := seen[region]; ok {
			return
		}
		seen[region] = struct{}{}
		regions = append(regions, region)
	}
	if env.identityErr == nil && env.client != nil {
		if enabled, err := env.client.ListEnabledRegions(env.ctx); err == nil {
			for _, region := range enabled {
				add(region)
			}
		}
	}
	if profiles, err := listAllConnections(env.store, env.configRoot); err == nil {
		for _, profile := range profiles {
			add(profile.Region)
		}
	}
	if env.client != nil {
		add(env.client.Region())
	}
	sort.Strings(regions)
	return regions
}

func newConnectionStartData(env *browserEnv) (ui.NewConnectionStart, error) {
	if env.identityErr != nil {
		return ui.NewConnectionStart{}, fmt.Errorf("new connection needs AWS connectivity: %w", env.identityErr)
	}
	regions, err := env.client.ListEnabledRegions(env.ctx)
	if err != nil {
		return ui.NewConnectionStart{}, fmt.Errorf("listing enabled AWS regions: %w", err)
	}
	if len(regions) == 0 {
		regions = []string{env.client.Region()}
	}
	return ui.NewConnectionStart{Regions: regions, AccountID: env.identity.AccountID, AWSProfile: env.awsProfile, SSHPort: env.sshPort}, nil
}

func scanNewCandidates(env *browserEnv, region string) ([]ui.RescanCandidate, error) {
	if env.identityErr != nil {
		return nil, fmt.Errorf("new connection needs AWS connectivity: %w", env.identityErr)
	}
	client, err := awsclient.New(env.ctx, region, env.awsProfile)
	if err != nil {
		return nil, err
	}
	identity, err := client.GetCallerIdentity(env.ctx)
	if err != nil {
		return nil, err
	}
	if identity.AccountID != env.identity.AccountID {
		return nil, fmt.Errorf("region %s resolved account %s instead of %s", region, identity.AccountID, env.identity.AccountID)
	}
	vpcs, _, err := client.ListTargetVPCs(env.ctx, env.vpcIDFlag)
	if err != nil {
		return nil, err
	}
	var candidates []probe.Candidate
	groupsByInstance := make(map[string][]string)
	groupSet := make(map[string]struct{})
	for _, vpc := range vpcs {
		instances, err := client.ListRunningInstances(env.ctx, vpc.VpcID)
		if err != nil {
			return nil, err
		}
		for _, instance := range instances {
			candidates = append(candidates, probe.Candidate{
				InstanceID: instance.InstanceID, Name: instance.NameTag, VpcID: vpc.VpcID,
				VpcCIDRs: vpc.CIDRBlocks, SubnetID: instance.SubnetID,
				PublicIP: instance.PublicIP, PrivateIP: instance.PrivateIP,
			})
			groupsByInstance[instance.InstanceID] = instance.SecurityGroupIDs
			for _, groupID := range instance.SecurityGroupIDs {
				groupSet[groupID] = struct{}{}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("no running instances were found in the selected region")
	}
	instanceIDs := make([]string, len(candidates))
	for index := range candidates {
		instanceIDs[index] = candidates[index].InstanceID
	}
	ssmOnline, ssmErr := client.ListSSMOnlineInstanceIDs(env.ctx, instanceIDs)
	if ssmErr != nil {
		ssmOnline = map[string]bool{}
	}
	probe.ProbeAll(candidates, env.sshPort, probeTimeout)
	for index := range candidates {
		if ssmOnline[candidates[index].InstanceID] {
			candidates[index].Method = probe.MethodSSM
			candidates[index].Target = candidates[index].InstanceID
			candidates[index].Reachable = true
		}
	}
	groupIDs := make([]string, 0, len(groupSet))
	for groupID := range groupSet {
		groupIDs = append(groupIDs, groupID)
	}
	egress, egressErr := client.DescribeSecurityGroupEgress(env.ctx, groupIDs)
	if egressErr == nil {
		for index := range candidates {
			var cidrs []string
			for _, groupID := range groupsByInstance[candidates[index].InstanceID] {
				for _, rule := range egress[groupID] {
					cidrs = append(cidrs, rule.CIDR)
				}
			}
			candidates[index].VpcEgressOK = len(candidates[index].VpcCIDRs) > 0
			for _, cidr := range candidates[index].VpcCIDRs {
				if !netcidr.CoversAny(cidrs, cidr) {
					candidates[index].VpcEgressOK = false
					break
				}
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return probe.Rank(candidates[i]) < probe.Rank(candidates[j]) })
	var result []ui.RescanCandidate
	for _, candidate := range candidates {
		if !candidate.Reachable {
			continue
		}
		result = append(result, ui.RescanCandidate{
			Name: candidate.Name, InstanceID: candidate.InstanceID, Method: candidate.Method,
			Target: candidate.Target, VpcID: candidate.VpcID, VpcCIDRs: candidate.VpcCIDRs,
			SubnetID: candidate.SubnetID, Reachable: true, EgressOK: candidate.VpcEgressOK,
		})
	}
	return result, nil
}

func prepareNewRoutes(env *browserEnv, region string, kind config.ProfileKind, candidate ui.RescanCandidate) ([]config.NetworkRoute, error) {
	routes := make([]config.NetworkRoute, 0, len(candidate.VpcCIDRs))
	seen := make(map[string]struct{})
	add := func(route config.NetworkRoute) error {
		cidr, err := netcidr.Normalize(route.CIDR)
		if err != nil {
			return err
		}
		if _, ok := seen[cidr]; ok {
			return nil
		}
		seen[cidr] = struct{}{}
		route.CIDR = cidr
		routes = append(routes, route)
		return nil
	}
	for _, cidr := range candidate.VpcCIDRs {
		if err := add(config.NetworkRoute{CIDR: cidr, Source: "pivot_vpc", Selected: true}); err != nil {
			return nil, err
		}
	}
	if kind == config.ProfileKindVPC {
		return routes, nil
	}
	client, err := awsclient.New(env.ctx, region, env.awsProfile)
	if err != nil {
		return nil, err
	}
	discovered, err := client.GetEffectiveSubnetRoutes(env.ctx, candidate.VpcID, candidate.SubnetID)
	if err != nil {
		return nil, fmt.Errorf("discovering subnet routes: %w", err)
	}
	for _, route := range discovered {
		if err := add(config.NetworkRoute{CIDR: route.CIDR, Source: "subnet_route", TargetType: route.TargetType, TargetID: route.TargetID}); err != nil {
			return nil, err
		}
	}
	return routes, nil
}

func browserActionResult(env *browserEnv, bin, action, profileName string) (ui.ActionResult, error) {
	profiles, err := listAllConnections(env.store, env.configRoot)
	if err != nil {
		return ui.ActionResult{}, err
	}
	var cfg *config.Config
	for _, profile := range profiles {
		if profile.Name == profileName {
			cfg = profile
			break
		}
	}
	if cfg == nil {
		return ui.ActionResult{}, fmt.Errorf("connection %q is no longer available", profileName)
	}
	accountID := cfg.AccountID
	if accountID == "" && env.identityErr == nil {
		accountID = env.identity.AccountID
	}

	switch action {
	case ui.ActStatus:
		return browserStatusResult(env.store, accountID, profileName)
	case ui.ActPrint:
		return browserPrintResult(cfg, bin)
	default:
		return ui.ActionResult{}, fmt.Errorf("action %q is not available in the UI", action)
	}
}

func browserStatusResult(store *config.Store, accountID, profileName string) (ui.ActionResult, error) {
	pidFile, err := profilePIDFile(store, accountID, profileName)
	if err != nil {
		return ui.ActionResult{}, err
	}
	pid, running, err := sshuttlecmd.BackgroundStatus(pidFile)
	if err != nil {
		return ui.ActionResult{}, fmt.Errorf("checking connection %s: %w", profileName, err)
	}
	status := "Not running"
	if running {
		status = fmt.Sprintf("Running (PID %d)", pid)
	}
	lines := []string{profileName + ": " + status}
	statePath, err := profileDNSStateFile(store, accountID, profileName)
	if err != nil {
		return ui.ActionResult{}, err
	}
	state, err := dnsresolver.LoadState(statePath)
	if err != nil {
		return ui.ActionResult{}, fmt.Errorf("reading split-DNS state: %w", err)
	}
	if state != nil {
		lines = append(lines, "DNS: "+state.Describe())
		if !running {
			lines = append(lines, "WARNING: resolver config is installed while the tunnel is down")
		}
	}
	return ui.ActionResult{Title: "Connection status", Body: strings.Join(lines, "\n")}, nil
}

func browserPrintResult(cfg *config.Config, bin string) (ui.ActionResult, error) {
	if bin == "" {
		return ui.ActionResult{}, errors.New("sshuttle was not found on PATH")
	}
	lines := []string{sshuttlecmd.CommandPreview(bin, sshuttlecmd.BuildArgs(cfg))}
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit {
		lines = append(lines,
			"",
			"Split-DNS setup:",
			strings.TrimSpace(dnsresolver.SetupInstructions(cfg)),
			"",
			"Split-DNS teardown:",
			strings.TrimSpace(dnsresolver.TeardownInstructions(cfg)),
		)
	}
	return ui.ActionResult{Title: "sshuttle command", Body: strings.Join(lines, "\n")}, nil
}

func browserConnectionSnapshot(env *browserEnv, bin, name string) (ui.ConnSummary, *ui.ConnDetail, error) {
	cfg, err := loadBrowserConfig(env, name)
	if err != nil {
		return ui.ConnSummary{}, nil, err
	}
	summary := ui.ConnSummary{
		Account: cfg.AccountID, Name: cfg.Name, Kind: string(cfg.Kind),
		Method: cfg.ConnectionMethod, Region: cfg.Region, AWSProfile: cfg.AWSProfile,
		Networks: strings.Join(cfg.SelectedCIDRs(), ", "), DNS: "off",
	}
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() != config.DNSModeOff {
		summary.DNS = cfg.DNSSummary()
	}
	if pid, running, statusErr := backgroundStatusQuick(env.store, cfg); statusErr != nil {
		return ui.ConnSummary{}, nil, statusErr
	} else if running {
		summary.Running, summary.PID = true, pid
	} else if len(cfg.SelectedCIDRs()) > 0 {
		summary.Reachable = statusReachableQuick(env, cfg)
		summary.Unreachable = !summary.Reachable
	}
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit && cfg.DNS.ManageResolver {
		statePath, err := profileDNSStateFile(env.store, cfg.AccountID, cfg.Name)
		if err != nil {
			return ui.ConnSummary{}, nil, err
		}
		state, err := dnsresolver.LoadState(statePath)
		if err != nil {
			return ui.ConnSummary{}, nil, err
		}
		summary.Stale = !summary.Running && state == nil
	}
	detail := buildDetail(env, name, bin)
	if detail == nil {
		return ui.ConnSummary{}, nil, fmt.Errorf("connection %q is no longer available", name)
	}
	return summary, detail, nil
}

func loadBrowserConfig(env *browserEnv, name string) (*config.Config, error) {
	profiles, err := listAllConnections(env.store, env.configRoot)
	if err != nil {
		return nil, err
	}
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, nil
		}
	}
	return nil, fmt.Errorf("connection %q is no longer available", name)
}

func browserStartResult(env *browserEnv, bin, name string) (ui.ActionResult, error) {
	if bin == "" {
		return ui.ActionResult{}, errors.New("sshuttle was not found on PATH")
	}
	cfg, err := loadBrowserConfig(env, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	pidFile, err := profilePIDFile(env.store, cfg.AccountID, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	if pid, running, err := sshuttlecmd.BackgroundStatus(pidFile); err != nil {
		return ui.ActionResult{}, err
	} else if running {
		return ui.ActionResult{Title: "Tunnel already running", Body: fmt.Sprintf("%s is running (PID %d).", name, pid)}, nil
	}
	statePath, err := profileDNSStateFile(env.store, cfg.AccountID, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	manageDNS := (cfg.DNS != nil && cfg.DNS.ManageResolver) || env.manageDNS
	var installed *dnsresolver.State
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit && manageDNS {
		options := dnsresolver.DefaultOptions()
		options.TerminalIn = false
		installed, err = options.Install(cfg, statePath)
		if err != nil {
			return ui.ActionResult{}, fmt.Errorf("installing split-DNS resolver noninteractively: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(env.ctx, 90*time.Second)
	defer cancel()
	// The child sshuttle spawns `aws ssm start-session` via the user's SSH
	// ProxyCommand; it must call the API in the connection's region, not in
	// the profile's default region (a US instance queried in eu-central-1
	// fails with TargetNotConnected even while its SSM agent is online).
	environment := map[string]string{
		"AWS_REGION":         cfg.Region,
		"AWS_DEFAULT_REGION": cfg.Region,
	}
	if profile := effectiveAWSProfile(cfg, env.awsProfile); profile != "" {
		environment["AWS_PROFILE"] = profile
	}
	var pid int
	var output string
	pid, output, startErr := sshuttlecmd.StartBackgroundCaptured(ctx, bin, sshuttlecmd.BuildArgs(cfg), pidFile, environment)
	if startErr != nil {
		if installed != nil {
			options := dnsresolver.DefaultOptions()
			options.TerminalIn = false
			_, cleanupErr := options.Remove(installed)
			_ = os.Remove(statePath)
			if cleanupErr != nil {
				startErr = errors.Join(startErr, fmt.Errorf("rolling back resolver setup: %w", cleanupErr))
			}
		}
		if strings.TrimSpace(output) != "" {
			startErr = fmt.Errorf("%w\n%s", startErr, strings.TrimSpace(output))
		}
		return ui.ActionResult{}, fmt.Errorf("starting %s: %w", name, startErr)
	}
	body := fmt.Sprintf("%s is running (PID %d).", name, pid)
	if strings.TrimSpace(output) != "" {
		body += "\n" + strings.TrimSpace(output)
	}
	return ui.ActionResult{Title: "Tunnel started", Body: body, Refresh: true}, nil
}

func browserLaunchStream(env *browserEnv, bin, name string) (*ui.LogSession, error) {
	if bin == "" {
		return nil, errors.New("sshuttle was not found on PATH")
	}
	cfg, err := loadBrowserConfig(env, name)
	if err != nil {
		return nil, err
	}
	statePath, err := profileDNSStateFile(env.store, cfg.AccountID, name)
	if err != nil {
		return nil, err
	}
	manageDNS := (cfg.DNS != nil && cfg.DNS.ManageResolver) || env.manageDNS
	var installed *dnsresolver.State
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit && manageDNS {
		options := dnsresolver.DefaultOptions()
		options.TerminalIn = false
		installed, err = options.Install(cfg, statePath)
		if err != nil {
			return nil, fmt.Errorf("installing split-DNS resolver noninteractively: %w", err)
		}
	}
	environment := map[string]string{
		"AWS_REGION":         cfg.Region,
		"AWS_DEFAULT_REGION": cfg.Region,
	}
	if profile := effectiveAWSProfile(cfg, env.awsProfile); profile != "" {
		environment["AWS_PROFILE"] = profile
	}
	process := sshuttlecmd.StartCommandStream(env.ctx, bin, sshuttlecmd.BuildArgs(cfg), environment)
	done := make(chan error, 1)
	go func() {
		var processErr error
		if process.Done != nil {
			processErr = <-process.Done
		}
		if installed != nil {
			if _, cleanupErr := removeBrowserDNSState(env.store, cfg.AccountID, name); cleanupErr != nil {
				processErr = errors.Join(processErr, fmt.Errorf("removing split-DNS resolver config: %w", cleanupErr))
			}
		}
		done <- processErr
		close(done)
	}()
	return &ui.LogSession{Lines: process.Lines, Done: done, Cancel: process.Cancel}, nil
}

func browserStopResult(env *browserEnv, name string) (ui.ActionResult, error) {
	cfg, err := loadBrowserConfig(env, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	pidFile, err := profilePIDFile(env.store, cfg.AccountID, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	pid, wasRunning, err := sshuttlecmd.StopBackground(pidFile)
	if err != nil {
		return ui.ActionResult{Title: "Stop attempted", Body: "Checking actual tunnel state…", Refresh: true}, fmt.Errorf("stopping %s: %w", name, err)
	}
	body := name + " was not running."
	if wasRunning {
		body = fmt.Sprintf("%s stopped (PID %d).", name, pid)
	}
	dnsMessage, err := removeBrowserDNSState(env.store, cfg.AccountID, name)
	if err != nil {
		return ui.ActionResult{Title: "Tunnel stopped; DNS cleanup failed", Body: body, Refresh: true}, fmt.Errorf("Split-DNS cleanup failed: %w", err)
	}
	if dnsMessage != "" {
		body += "\n" + dnsMessage
	}
	return ui.ActionResult{Title: "Tunnel stopped", Body: body, Refresh: true}, nil
}

func browserForgetResult(env *browserEnv, name string) (ui.ActionResult, error) {
	cfg, err := loadBrowserConfig(env, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	pidFile, err := profilePIDFile(env.store, cfg.AccountID, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	if _, _, err := sshuttlecmd.StopBackground(pidFile); err != nil {
		return ui.ActionResult{}, fmt.Errorf("stopping %s before forgetting it: %w", name, err)
	}
	if _, err := removeBrowserDNSState(env.store, cfg.AccountID, name); err != nil {
		return ui.ActionResult{}, fmt.Errorf("connection was not removed because DNS cleanup failed: %w", err)
	}
	if err := env.store.Delete(cfg.AccountID, name); err != nil {
		return ui.ActionResult{}, fmt.Errorf("removing connection %s: %w", name, err)
	}
	return ui.ActionResult{Title: "Connection forgotten", Body: name + " was removed.", RemoveConnection: true}, nil
}

func browserDNSSetupResult(env *browserEnv, name string) (ui.ActionResult, error) {
	cfg, err := loadBrowserConfig(env, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	if cfg.DNS == nil || cfg.DNS.EffectiveMode() != config.DNSModeSplit {
		return ui.ActionResult{}, errors.New("DNS resolver install requires a connection in split mode")
	}
	statePath, err := profileDNSStateFile(env.store, cfg.AccountID, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	options := dnsresolver.DefaultOptions()
	options.TerminalIn = false
	state, err := options.Install(cfg, statePath)
	if err != nil {
		return ui.ActionResult{}, fmt.Errorf("installing split-DNS resolver noninteractively: %w", err)
	}
	return ui.ActionResult{Title: "Split DNS installed", Body: state.Describe(), Refresh: true}, nil
}

func browserDNSTeardownResult(env *browserEnv, name string) (ui.ActionResult, error) {
	cfg, err := loadBrowserConfig(env, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	body, err := removeBrowserDNSState(env.store, cfg.AccountID, name)
	if err != nil {
		return ui.ActionResult{}, err
	}
	if body == "" {
		body = "No split-DNS resolver config was installed."
	}
	return ui.ActionResult{Title: "Split DNS removed", Body: body, Refresh: true}, nil
}

func removeBrowserDNSState(store *config.Store, accountID, name string) (string, error) {
	statePath, err := profileDNSStateFile(store, accountID, name)
	if err != nil {
		return "", err
	}
	state, err := dnsresolver.LoadState(statePath)
	if err != nil {
		return "", err
	}
	if state == nil {
		return "", nil
	}
	options := dnsresolver.DefaultOptions()
	options.TerminalIn = false
	warnings, err := options.Remove(state)
	if err != nil {
		return "", err
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return strings.Join(warnings, "\n"), nil
}

func reconcileDNSInBrowser(store *config.Store, accountID, name string, cfg *config.Config, manageOverride bool) error {
	statePath, err := profileDNSStateFile(store, accountID, name)
	if err != nil {
		return err
	}
	installed, err := dnsresolver.LoadState(statePath)
	if err != nil {
		return err
	}
	managedSplit := cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit && (cfg.DNS.ManageResolver || manageOverride)
	options := dnsresolver.DefaultOptions()
	options.TerminalIn = false
	if managedSplit {
		_, err := options.Install(cfg, statePath)
		return err
	}
	if installed == nil {
		return nil
	}
	warnings, err := options.Remove(installed)
	if err != nil {
		return err
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(warnings) > 0 {
		return fmt.Errorf("resolver cleanup warnings: %s", strings.Join(warnings, "; "))
	}
	return nil
}

func checkConnectionAWSProfile(parent context.Context, cfg *config.Config, profile string) (ui.ActionResult, error) {
	if cfg == nil {
		return ui.ActionResult{}, errors.New("connection configuration is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	client, err := awsclient.New(ctx, cfg.Region, profile)
	if err != nil {
		return ui.ActionResult{}, fmt.Errorf("loading AWS profile %q: %w", profile, err)
	}
	identity, err := client.GetCallerIdentity(ctx)
	if err != nil {
		return ui.ActionResult{}, fmt.Errorf("checking AWS profile %q: %w", profile, err)
	}
	if cfg.AccountID != "" && identity.AccountID != cfg.AccountID {
		return ui.ActionResult{}, fmt.Errorf("profile %q authenticated to account %s; connection belongs to %s", profile, identity.AccountID, cfg.AccountID)
	}
	lines := []string{fmt.Sprintf("Profile %q authenticated to account %s.", profile, identity.AccountID)}
	if cfg.ConnectionMethod != probe.MethodSSM {
		lines = append(lines, "Direct-IP tunnels do not use AWS SSM at runtime.")
		return ui.ActionResult{Title: "AWS profile check", Body: strings.Join(lines, "\n")}, nil
	}
	instanceID := cfg.InstanceID
	if instanceID == "" {
		instanceID = cfg.Target
	}
	if instanceID == "" {
		return ui.ActionResult{}, errors.New("connection has no SSM instance ID to check")
	}
	online, err := client.ListSSMOnlineInstanceIDs(ctx, []string{instanceID})
	if err != nil {
		return ui.ActionResult{}, fmt.Errorf("checking SSM managed-instance status: %w", err)
	}
	if online[instanceID] {
		lines = append(lines, fmt.Sprintf("SSM reports %s online.", instanceID))
	} else {
		lines = append(lines, fmt.Sprintf("SSM does not report %s online.", instanceID))
	}
	lines = append(lines, "This non-invasive check does not verify ssm:StartSession permission.")
	return ui.ActionResult{Title: "AWS profile readiness", Body: strings.Join(lines, "\n")}, nil
}

func withAWSProfile(profile string, run func()) {
	if err := withAWSProfileErr(profile, func() error { run(); return nil }); err != nil {
		fatal(err)
	}
}

func withAWSProfileErr(profile string, run func() error) error {
	if profile == "" {
		return run()
	}
	previous, existed := os.LookupEnv("AWS_PROFILE")
	if err := os.Setenv("AWS_PROFILE", profile); err != nil {
		return fmt.Errorf("setting AWS_PROFILE: %w", err)
	}
	defer func() {
		if existed {
			_ = os.Setenv("AWS_PROFILE", previous)
		} else {
			_ = os.Unsetenv("AWS_PROFILE")
		}
	}()
	return run()
}

func effectiveAWSProfile(cfg *config.Config, override string) string {
	if override != "" {
		return override
	}
	if cfg != nil {
		return cfg.AWSProfile
	}
	return ""
}

// containsConnection reports whether name is still in the list.
func containsConnection(summaries []ui.ConnSummary, name string) bool {
	for _, summary := range summaries {
		if summary.Name == name {
			return true
		}
	}
	return false
}

// gatherSummaries walks every account's cached connections and builds the
// home-table rows: pid-file status, DNS state, staleness, and (when online)
// a bounded parallel reachability probe per non-running connection.
func gatherSummaries(env *browserEnv) []ui.ConnSummary {
	profiles, err := listAllConnections(env.store, env.configRoot)
	if err != nil {
		fatal(err)
	}
	online := env.identityErr == nil

	reach := make([]bool, len(profiles))
	var wg sync.WaitGroup
	for index, profile := range profiles {
		if profile == nil || !online || len(profile.SelectedCIDRs()) == 0 {
			continue
		}
		if runningQuick(env.store, profile) {
			continue
		}
		wg.Add(1)
		go func(index int, profile *config.Config) {
			defer wg.Done()
			reach[index] = statusReachableQuick(env, profile)
		}(index, profile)
	}
	wg.Wait()

	summaries := make([]ui.ConnSummary, 0, len(profiles))
	for index, profile := range profiles {
		summary := ui.ConnSummary{
			Account:    profile.AccountID,
			Name:       profile.Name,
			Kind:       string(profile.Kind),
			Method:     profile.ConnectionMethod,
			Region:     profile.Region,
			AWSProfile: profile.AWSProfile,
			Networks:   strings.Join(profile.SelectedCIDRs(), ", "),
		}
		if profile.DNS != nil && profile.DNS.EffectiveMode() != config.DNSModeOff {
			summary.DNS = profile.DNSSummary()
		} else {
			summary.DNS = "off"
		}
		if pid, running, statusErr := backgroundStatusQuick(env.store, profile); statusErr == nil && running {
			summary.Running = true
			summary.PID = pid
			// Resolver config missing while auto-management expects it.
			if profile.DNS != nil && profile.DNS.EffectiveMode() == config.DNSModeSplit && profile.DNS.ManageResolver {
				statePath, pathErr := profileDNSStateFile(env.store, profile.AccountID, profile.Name)
				if pathErr == nil {
					state, stateErr := dnsresolver.LoadState(statePath)
					summary.Stale = stateErr == nil && state == nil
				}
			}
		} else if online {
			summary.Reachable = reach[index]
			summary.Unreachable = !reach[index]
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

// listAllConnections loads every connection across all saved accounts.
// Each YAML carries its own account id, so this works offline too.
func listAllConnections(store *config.Store, configRoot string) ([]*config.Config, error) {
	accountsDir := filepath.Join(configRoot, "accounts")
	entries, err := os.ReadDir(accountsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing account directories: %w", err)
	}
	var all []*config.Config
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		profiles, err := store.List(entry.Name())
		if err != nil {
			return nil, err
		}
		all = append(all, profiles...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

func connectionNames(profiles []*config.Config) string {
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, profile.Name)
	}
	return strings.Join(names, ", ")
}

func runningQuick(store *config.Store, profile *config.Config) bool {
	pidFile, err := profilePIDFile(store, profile.AccountID, profile.Name)
	if err != nil {
		return false
	}
	_, running, err := sshuttlecmd.BackgroundStatus(pidFile)
	return err == nil && running
}

func backgroundStatusQuick(store *config.Store, profile *config.Config) (int, bool, error) {
	pidFile, err := profilePIDFile(store, profile.AccountID, profile.Name)
	if err != nil {
		return 0, false, err
	}
	return sshuttlecmd.BackgroundStatus(pidFile)
}

// verifyTargetQuick is the bounded reachability probe used for status
// displays (the full verifyTarget timeout would make the table crawl).
func verifyTargetQuick(method, target string, port int) bool {
	if method == probe.MethodSSM {
		ok, _ := sshtest.TryConnect(target, port, quickProbeTimeout)
		return ok
	}
	ok, _ := probe.ProbeSSH(target, port, quickProbeTimeout)
	return ok
}

// statusReachableQuick uses the same SSM check as the v shortcut, since an ssh probe can time out behind SSM ProxyCommand.
func statusReachableQuick(env *browserEnv, cfg *config.Config) bool {
	if cfg.ConnectionMethod != probe.MethodSSM {
		return verifyTargetQuick(cfg.ConnectionMethod, cfg.Target, cfg.SSHPort)
	}
	instanceID := cfg.InstanceID
	if instanceID == "" {
		instanceID = cfg.Target
	}
	if instanceID == "" {
		return false
	}
	profile := effectiveAWSProfile(cfg, env.awsProfile)
	if profile == "" {
		profile = "default"
	}
	ctx, cancel := context.WithTimeout(env.ctx, 15*time.Second)
	defer cancel()
	client, err := awsclient.New(ctx, cfg.Region, profile)
	if err != nil {
		return false
	}
	online, err := client.ListSSMOnlineInstanceIDs(ctx, []string{instanceID})
	return err == nil && online[instanceID]
}

const quickProbeTimeout = 3 * time.Second

// buildDetail assembles the Level-1 card data for one connection.
func buildDetail(env *browserEnv, profileName string, bin string) *ui.ConnDetail {
	profiles, err := listAllConnections(env.store, env.configRoot)
	if err != nil {
		fatal(err)
	}
	var cfg *config.Config
	for _, profile := range profiles {
		if profile.Name == profileName {
			cfg = profile
			break
		}
	}
	if cfg == nil {
		return nil
	}

	detail := &ui.ConnDetail{
		Name:       cfg.Name,
		Kind:       string(cfg.Kind),
		Method:     cfg.ConnectionMethod,
		Region:     cfg.Region,
		AWSProfile: cfg.AWSProfile,
		SSHPort:    cfg.SSHPort,
		Target:     cfg.Target,
		Routes:     cfg.Routes,
		Networks:   cfg.SelectedCIDRs(),
	}
	if cfg.DNS != nil {
		detail.Domains = cfg.DNS.Domains
		detail.Resolver = cfg.DNS.Resolver
		if cfg.DNS.EffectiveMode() != config.DNSModeOff {
			detail.DNSSummary = cfg.DNSSummary()
		}
	}
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() != config.DNSModeOff {
		detail.DNSSummary = cfg.DNSSummary()
	}
	if bin != "" {
		detail.Command = sshuttlecmd.CommandPreview(bin, sshuttlecmd.BuildArgs(cfg))
	}

	if pid, running, _ := backgroundStatusQuick(env.store, cfg); running {
		detail.Running = true
		detail.PID = pid
	}
	// Resolver installation state from the manifest.
	if cfg.DNS != nil && cfg.DNS.EffectiveMode() == config.DNSModeSplit {
		statePath, pathErr := profileDNSStateFile(env.store, cfg.AccountID, cfg.Name)
		if pathErr == nil {
			state, _ := dnsresolver.LoadState(statePath)
			if state != nil {
				detail.DNSInstalled = state.Describe()
			} else {
				detail.DNSWantsInstall = true
			}
		}
	}
	// Reachability: skipped while the tunnel runs (target is reached through
	// sshuttle) and while offline.
	if !detail.Running && env.identityErr == nil {
		detail.TargetOK = verifyTargetQuick(cfg.ConnectionMethod, cfg.Target, cfg.SSHPort)
		if !detail.TargetOK {
			detail.ReachDetail = fmt.Sprintf("not reachable now (probed %ds) — rescan (r) may pick a new target", int(quickProbeTimeout.Seconds()))
		}
	} else if detail.Running {
		detail.TargetOK = true
	}
	return detail
}

// executeBrowserAction maps one Outcome to the classic single-shot action
// functions so hotkey and CLI behavior never drift apart.
func executeBrowserAction(env *browserEnv, bin string, action string, profileName string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if failure, ok := recovered.(browserFailure); ok {
				err = failure.err
				return
			}
			panic(recovered)
		}
	}()
	profiles, err := listAllConnections(env.store, env.configRoot)
	if err != nil {
		return err
	}
	var cfg *config.Config
	for _, profile := range profiles {
		if profile.Name == profileName {
			cfg = profile
			break
		}
	}
	if cfg == nil {
		return fmt.Errorf("connection %q is gone; nothing to do", profileName)
	}
	accountID := cfg.AccountID
	if accountID == "" && env.identityErr == nil {
		accountID = env.identity.AccountID
	}
	store := env.store
	var actionErr error

	switch action {
	case ui.ActStart:
		if bin == "" {
			fmt.Fprintln(os.Stderr, "WARNING: sshuttle not found on PATH; cannot start")
			return fmt.Errorf("sshuttle not found on PATH; cannot start")
		}
		profile := effectiveAWSProfile(cfg, env.awsProfile)
		withAWSProfile(profile, func() {
			actionStartTunnel(store, accountID, profileName, cfg, bin, (cfg.DNS != nil && cfg.DNS.ManageResolver) || env.manageDNS)
		})
	case ui.ActLaunch:
		if bin == "" {
			fmt.Fprintln(os.Stderr, "WARNING: sshuttle not found on PATH; cannot start")
			return fmt.Errorf("sshuttle not found on PATH; cannot launch")
		}
		profile := effectiveAWSProfile(cfg, env.awsProfile)
		withAWSProfile(profile, func() {
			actionLaunchTunnel(store, accountID, profileName, cfg, bin, (cfg.DNS != nil && cfg.DNS.ManageResolver) || env.manageDNS)
		})
	case ui.ActStop:
		actionStop(store, accountID, profileName)
	case ui.ActStatus:
		actionStatus(store, accountID, profileName)
	case ui.ActLogs:
		followLogsAction(true)
	case ui.ActPrint:
		if bin == "" {
			fmt.Fprintln(os.Stderr, "WARNING: sshuttle not found on PATH")
			return fmt.Errorf("sshuttle not found on PATH")
		}
		actionPrintCommand(cfg, bin)
	case ui.ActDNSSetup:
		actionDNSSetup(store, accountID, profileName, cfg)
	case ui.ActDNSTeardown:
		actionDNSTeardown(store, accountID, profileName)
	case ui.ActDNSEdit:
		editDNSInMenu(store, accountID, profileName, cfg, env.manageDNS)
	case ui.ActRescan:
		actionErr = rescanConnection(env, profileName, cfg)
	case ui.ActForget:
		actionForget(store, accountID, profileName)
		return
	default:
		return fmt.Errorf("unhandled action %q", action)
	}
	ui.Pause("Press Enter to return…")
	return actionErr
}

// rescanConnection re-runs discovery for one saved connection and saves the
// result (the manage-menu "Rescan" behavior).
func rescanConnection(env *browserEnv, profileName string, cfg *config.Config) error {
	if cfg == nil || profileName == "" {
		return errors.New("no saved connection selected for rescan")
	}
	if env.identityErr != nil {
		return fmt.Errorf("rescan needs AWS connectivity: %w", env.identityErr)
	}
	awsProfile := effectiveAWSProfile(cfg, env.awsProfile)
	client, err := awsclient.New(env.ctx, cfg.Region, awsProfile)
	if err != nil {
		return fmt.Errorf("opening AWS profile for rescan: %w", err)
	}
	identity, err := client.GetCallerIdentity(env.ctx)
	if err != nil {
		return fmt.Errorf("checking AWS identity for rescan: %w", err)
	}
	if cfg.AccountID != "" && identity.AccountID != cfg.AccountID {
		return fmt.Errorf("AWS profile %q resolves to account %s; connection belongs to %s", awsProfile, identity.AccountID, cfg.AccountID)
	}
	kind := cfg.Kind
	if kind == "" {
		kind = config.ProfileKindVPC
	}
	var newCfg *config.Config
	var scanErr error
	withAWSProfile(awsProfile, func() {
		newCfg, scanErr = runDiscoveryFlow(env.ctx, client, identity, "", env.sshPort, kind, awsProfile, cfg.DNS, nil, cfg.InstanceID)
	})
	if scanErr != nil {
		return fmt.Errorf("rescan failed: %w", scanErr)
	}
	if newCfg == nil {
		return nil
	}
	if err := saveRescannedConfig(env.store, cfg, profileName, newCfg); err != nil {
		return fmt.Errorf("saving rescanned connection %q: %w", profileName, err)
	}
	fmt.Printf("Saved connection %q with pivot %s.\n", profileName, newCfg.InstanceID)
	reconcileDNS(env.store, cfg.AccountID, profileName, newCfg, env.manageDNS)
	return nil
}

func saveRescannedConfig(store *config.Store, existing *config.Config, name string, updated *config.Config) error {
	if store == nil || existing == nil || updated == nil || name == "" {
		return errors.New("cannot save rescan without an existing connection and selected pivot")
	}
	updated.Name = name
	updated.AccountID = existing.AccountID
	if existing.SavedAt != "" {
		updated.SavedAt = existing.SavedAt
	}
	return store.Save(existing.AccountID, name, updated)
}

func prepareRescanCandidates(env *browserEnv, profileName string) ([]ui.RescanCandidate, error) {
	if env.identityErr != nil {
		return nil, fmt.Errorf("rescan needs AWS connectivity: %w", env.identityErr)
	}
	profiles, err := listAllConnections(env.store, env.configRoot)
	if err != nil {
		return nil, err
	}
	var cfg *config.Config
	for _, profile := range profiles {
		if profile.Name == profileName {
			cfg = profile
			break
		}
	}
	if cfg == nil {
		return nil, fmt.Errorf("connection %q is no longer available", profileName)
	}
	awsProfile := effectiveAWSProfile(cfg, env.awsProfile)
	client, err := awsclient.New(env.ctx, cfg.Region, awsProfile)
	if err != nil {
		return nil, fmt.Errorf("opening AWS profile for rescan: %w", err)
	}
	identity, err := client.GetCallerIdentity(env.ctx)
	if err != nil {
		return nil, fmt.Errorf("checking AWS identity for rescan: %w", err)
	}
	if cfg.AccountID != "" && identity.AccountID != cfg.AccountID {
		return nil, fmt.Errorf("AWS profile %q resolves to account %s; connection belongs to %s", awsProfile, identity.AccountID, cfg.AccountID)
	}
	vpcs, _, err := client.ListTargetVPCs(env.ctx, cfg.VpcID)
	if err != nil {
		return nil, err
	}
	if len(vpcs) == 0 {
		return nil, fmt.Errorf("VPC %s is no longer available", cfg.VpcID)
	}
	instances, err := client.ListRunningInstances(env.ctx, cfg.VpcID)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("no running instances found in VPC %s", cfg.VpcID)
	}
	candidates := make([]probe.Candidate, len(instances))
	groupIDs := make([]string, 0)
	groupSet := make(map[string]struct{})
	instanceIDs := make([]string, len(instances))
	for index, instance := range instances {
		candidates[index] = probe.Candidate{
			InstanceID: instance.InstanceID,
			Name:       instance.NameTag,
			VpcID:      cfg.VpcID,
			VpcCIDRs:   vpcs[0].CIDRBlocks,
			SubnetID:   instance.SubnetID,
			PublicIP:   instance.PublicIP,
			PrivateIP:  instance.PrivateIP,
		}
		instanceIDs[index] = instance.InstanceID
		for _, groupID := range instance.SecurityGroupIDs {
			if _, ok := groupSet[groupID]; !ok {
				groupSet[groupID] = struct{}{}
				groupIDs = append(groupIDs, groupID)
			}
		}
	}
	ssmOnline, err := client.ListSSMOnlineInstanceIDs(env.ctx, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("checking SSM managed instances: %w", err)
	}
	probe.ProbeAll(candidates, cfg.SSHPort, probeTimeout)
	for index := range candidates {
		if ssmOnline[candidates[index].InstanceID] {
			candidates[index].Method = probe.MethodSSM
			candidates[index].Reachable = true
			candidates[index].Target = candidates[index].InstanceID
		}
	}
	egress, egressErr := client.DescribeSecurityGroupEgress(env.ctx, groupIDs)
	if egressErr == nil {
		for index := range candidates {
			var cidrs []string
			for _, instance := range instances {
				if instance.InstanceID == candidates[index].InstanceID {
					for _, groupID := range instance.SecurityGroupIDs {
						for _, rule := range egress[groupID] {
							cidrs = append(cidrs, rule.CIDR)
						}
					}
					break
				}
			}
			candidates[index].VpcEgressOK = len(candidates[index].VpcCIDRs) > 0
			for _, cidr := range candidates[index].VpcCIDRs {
				if !netcidr.CoversAny(cidrs, cidr) {
					candidates[index].VpcEgressOK = false
					break
				}
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return probe.Rank(candidates[i]) < probe.Rank(candidates[j]) })
	candidates = prioritizeAlternativeCandidate(candidates, cfg.InstanceID)
	result := make([]ui.RescanCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Reachable {
			continue
		}
		result = append(result, ui.RescanCandidate{
			Name: candidate.Name, InstanceID: candidate.InstanceID, Method: candidate.Method,
			Target: candidate.Target, VpcID: candidate.VpcID, VpcCIDRs: candidate.VpcCIDRs,
			SubnetID: candidate.SubnetID, Reachable: candidate.Reachable,
			EgressOK: candidate.VpcEgressOK, Current: candidate.InstanceID == cfg.InstanceID,
		})
	}
	return result, nil
}

func saveRescanCandidate(env *browserEnv, profileName string, candidate ui.RescanCandidate) (*config.Config, error) {
	profiles, err := listAllConnections(env.store, env.configRoot)
	if err != nil {
		return nil, err
	}
	var existing *config.Config
	for _, profile := range profiles {
		if profile.Name == profileName {
			existing = profile
			break
		}
	}
	if existing == nil {
		return nil, fmt.Errorf("connection %q is no longer available", profileName)
	}
	updated := *existing
	updated.InstanceID = candidate.InstanceID
	updated.NameTag = candidate.Name
	updated.SubnetID = candidate.SubnetID
	updated.ConnectionMethod = candidate.Method
	updated.Target = candidate.Target
	updated.VpcID = candidate.VpcID
	updated.VpcCIDRs = append([]string(nil), candidate.VpcCIDRs...)
	updated.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveRescannedConfig(env.store, existing, profileName, &updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// discoverNewConnection runs the full new-connection flow extracted from
// main(): region selection, kind prompt, discovery, save. It duplicates the
// scripted path closely so hotkey "new" behaves like a fresh invocation.
func discoverNewConnection(env *browserEnv, profileName string, prevCfg *config.Config) (*config.Config, error) {
	if env.identityErr != nil {
		return nil, fmt.Errorf("scanning needs AWS connectivity: %w", env.identityErr)
	}
	client := env.client
	identity := env.identity

	selectedRegion := env.regionFlag
	if prevCfg != nil {
		if selectedRegion != "" && selectedRegion != prevCfg.Region {
			return nil, fmt.Errorf("connection %q belongs to region %s; rescan instead", prevCfg.Name, prevCfg.Region)
		}
		selectedRegion = prevCfg.Region
	} else if selectedRegion == "" {
		regions, err := client.ListEnabledRegions(env.ctx)
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
				return nil, err
			}
			if canceled {
				return nil, nil
			}
			selectedRegion = regions[selected]
		}
	}
	if selectedRegion != client.Region() {
		regionClient, err := awsclient.New(env.ctx, selectedRegion, env.awsProfile)
		if err != nil {
			return nil, err
		}
		selectedIdentity, err := regionClient.GetCallerIdentity(env.ctx)
		if err != nil {
			return nil, err
		}
		if selectedIdentity.AccountID != identity.AccountID {
			return nil, fmt.Errorf("region client resolved account %s instead of %s", selectedIdentity.AccountID, identity.AccountID)
		}
		client = regionClient
	}
	// The SSM SSH ProxyCommand runs the AWS CLI outside the Go SDK. Keep it
	// on the same region/profile as discovery and the saved connection.
	if err := os.Setenv("AWS_REGION", selectedRegion); err != nil {
		return nil, fmt.Errorf("setting AWS_REGION: %w", err)
	}
	if err := os.Setenv("AWS_DEFAULT_REGION", selectedRegion); err != nil {
		return nil, fmt.Errorf("setting AWS_DEFAULT_REGION: %w", err)
	}
	if env.awsProfile != "" {
		if err := os.Setenv("AWS_PROFILE", env.awsProfile); err != nil {
			return nil, fmt.Errorf("setting AWS_PROFILE: %w", err)
		}
	}
	fmt.Println("Region:", client.Region())

	if profileName == "" {
		for {
			profileName = ui.PromptString("Connection name", "default")
			if err := config.ValidateProfileName(profileName); err == nil {
				break
			} else {
				fmt.Println(err)
			}
		}
	} else if config.ValidateProfileName(profileName) != nil {
		return nil, fmt.Errorf("invalid connection name %q", profileName)
	}

	kind := config.ProfileKindVPC
	if prevCfg != nil && prevCfg.Kind != "" {
		kind = prevCfg.Kind
	} else {
		selected, canceled, err := ui.Select("Connection type", []ui.Choice{
			{Title: "VPC", Description: "Tunnel the pivot VPC CIDRs"},
			{Title: "Dedicated network", Description: "Choose subnet routes and custom CIDRs"},
		})
		if err != nil {
			return nil, err
		}
		if canceled {
			return nil, nil
		}
		if selected == 1 {
			kind = config.ProfileKindDedicated
		}
	}

	var prevDNS *config.DNSConfig
	if prevCfg != nil {
		prevDNS = prevCfg.DNS
	}
	dnsPreset := dnsPresetFromFlags(env.dnsModeFlag, env.dnsDomains, env.dnsResolver, env.dnsToNS)

	newCfg, err := runDiscoveryFlow(env.ctx, client, identity, env.vpcIDFlag, env.sshPort, kind, env.awsProfile, prevDNS, dnsPreset, "")
	if err != nil {
		return nil, err
	}
	if newCfg == nil {
		return nil, nil
	}
	if prevCfg != nil && prevCfg.SavedAt != "" {
		newCfg.SavedAt = prevCfg.SavedAt
	}
	accountID := identity.AccountID
	if newCfg.AccountID != "" {
		accountID = newCfg.AccountID
	}
	if err := env.store.Save(accountID, profileName, newCfg); err != nil {
		return nil, fmt.Errorf("saving config: %w", err)
	}
	fmt.Printf("Saved connection %q.\n", profileName)
	reconcileDNS(env.store, accountID, profileName, newCfg, env.manageDNS)
	return newCfg, nil
}

// editDNSInMenu re-runs the interactive DNS prompts against the loaded
// connection, persists the result, and reconciles any installed resolver
// configuration.
func editDNSInMenu(store *config.Store, accountID, profileName string, cfg *config.Config, manageDNSOverride bool) {
	if cfg == nil || profileName == "" {
		fatal(errors.New("no saved connection selected"))
	}
	dns, err := promptDNS(cfg.DNS, nil, detectVPCResolver(cfg.VpcCIDRs), cfg.SelectedCIDRs())
	if err != nil {
		fatal(err)
	}
	if dns == nil {
		return
	}
	if err := dns.Normalize(); err != nil {
		fatal(err)
	}
	cfg.DNS = dns
	if err := store.Save(accountID, profileName, cfg); err != nil {
		fatal(fmt.Errorf("saving connection DNS settings: %w", err))
	}
	fmt.Printf("Updated DNS settings for connection %q (mode: %s).\n", profileName, cfg.DNS.EffectiveMode())
	reconcileDNS(store, accountID, profileName, cfg, manageDNSOverride)
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
//
// prevDNS prefills the DNS prompts from an existing connection (nil for
// first-time discovery); dnsPreset carries any --dns* CLI flags.
func prioritizeAlternativeCandidate(candidates []probe.Candidate, currentInstanceID string) []probe.Candidate {
	if currentInstanceID == "" || len(candidates) < 2 {
		return candidates
	}
	currentIndex := -1
	for index, candidate := range candidates {
		if candidate.InstanceID == currentInstanceID {
			currentIndex = index
			break
		}
	}
	if currentIndex < 0 {
		return candidates
	}
	ordered := make([]probe.Candidate, 0, len(candidates))
	ordered = append(ordered, candidates[:currentIndex]...)
	ordered = append(ordered, candidates[currentIndex+1:]...)
	ordered = append(ordered, candidates[currentIndex])
	return ordered
}

func runDiscoveryFlow(ctx context.Context, client *awsclient.Client, identity *awsclient.Identity, vpcID string, sshPort int, kind config.ProfileKind, awsProfile string, prevDNS *config.DNSConfig, dnsPreset *config.DNSConfig, currentInstanceID string) (*config.Config, error) {
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
	if currentInstanceID != "" {
		candidates = prioritizeAlternativeCandidate(candidates, currentInstanceID)
	}

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

	idx, canceled, err := ui.SelectCandidate(candidates, currentInstanceID)
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

	dns, err := promptDNS(prevDNS, dnsPreset, detectVPCResolver(chosen.VpcCIDRs), selectedCIDRs(routes))
	if err != nil {
		return nil, err
	}
	if dns == nil {
		return nil, nil
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
		DNS:              dns,
		SavedAt:          now,
		UpdatedAt:        now,
	}
	return cfg, nil
}

// selectedCIDRs returns the CIDRs the user kept in the route selector.
func selectedCIDRs(routes []config.NetworkRoute) []string {
	var cidrs []string
	for _, route := range routes {
		if route.Selected {
			cidrs = append(cidrs, route.CIDR)
		}
	}
	return cidrs
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
	if browserInteractive {
		panic(browserFailure{err: err})
	}
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}
