package main

import (
	"strings"
	"testing"

	"bastionctl/internal/config"
	"bastionctl/internal/probe"
	"bastionctl/internal/ui"
)

func TestBrowserStatusResultForStoppedConnection(t *testing.T) {
	store := config.NewStore(t.TempDir())
	result, err := browserStatusResult(store, "123456789012", "corp")
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "Connection status" || !strings.Contains(result.Body, "corp: Not running") {
		t.Fatalf("result = %+v", result)
	}
}

func TestBrowserPrintResult(t *testing.T) {
	cfg := &config.Config{
		Name:    "corp",
		Target:  "bastion.example.com",
		SSHPort: 22,
		Routes:  []config.NetworkRoute{{CIDR: "10.0.0.0/16", Selected: true}},
	}
	result, err := browserPrintResult(cfg, "/usr/local/bin/sshuttle")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"/usr/local/bin/sshuttle", "bastion.example.com", "10.0.0.0/16"} {
		if !strings.Contains(result.Body, expected) {
			t.Fatalf("command result missing %q: %+v", expected, result)
		}
	}
	if _, err := browserPrintResult(cfg, ""); err == nil {
		t.Fatal("missing sshuttle binary must return an error")
	}
}

func TestFormatAccount(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		alias     string
		want      string
	}{
		{name: "alias", accountID: "123456789012", alias: "production", want: "production [123456789012]"},
		{name: "id only", accountID: "123456789012", want: "123456789012"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatAccount(test.accountID, test.alias); got != test.want {
				t.Fatalf("formatAccount() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEffectiveAWSProfilePrefersExplicitOverride(t *testing.T) {
	cfg := &config.Config{AWSProfile: "connection-profile"}
	if got := effectiveAWSProfile(cfg, "flag-profile"); got != "flag-profile" {
		t.Fatalf("explicit profile = %q, want flag-profile", got)
	}
	if got := effectiveAWSProfile(cfg, ""); got != "connection-profile" {
		t.Fatalf("saved profile = %q, want connection-profile", got)
	}
	if got := effectiveAWSProfile(&config.Config{}, ""); got != "" {
		t.Fatalf("default profile = %q, want empty AWS default", got)
	}
}

func TestRescanPrioritizesAlternativePivot(t *testing.T) {
	candidates := []probe.Candidate{
		{InstanceID: "i-current"},
		{InstanceID: "i-alternative-1"},
		{InstanceID: "i-alternative-2"},
	}
	got := prioritizeAlternativeCandidate(candidates, "i-current")
	if got[0].InstanceID != "i-alternative-1" || got[len(got)-1].InstanceID != "i-current" {
		t.Fatalf("rescan candidates order = %+v", got)
	}
	unchanged := prioritizeAlternativeCandidate(candidates, "i-not-found")
	if unchanged[0].InstanceID != "i-current" {
		t.Fatalf("unknown current pivot changed candidate ordering: %+v", unchanged)
	}
}

func TestSaveRescannedConfigReplacesYamlPivot(t *testing.T) {
	store := config.NewStore(t.TempDir())
	existing := &config.Config{
		Name: "analytics.de.default", AccountID: "698715940080", Region: "eu-central-1",
		InstanceID: "i-old", Target: "i-old", SavedAt: "2026-01-01T00:00:00Z",
	}
	if err := store.Save(existing.AccountID, existing.Name, existing); err != nil {
		t.Fatal(err)
	}
	updated := &config.Config{Region: "eu-central-1", InstanceID: "i-new", Target: "i-new"}
	if err := saveRescannedConfig(store, existing, existing.Name, updated); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(existing.AccountID, existing.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InstanceID != "i-new" || loaded.Target != "i-new" || loaded.SavedAt != existing.SavedAt {
		t.Fatalf("saved rescan config = %+v", loaded)
	}
}

func TestSplitFields(t *testing.T) {
	if got := splitFields("corp.internal, svc.cluster.local\tza.internal"); len(got) != 3 {
		t.Fatalf("splitFields = %#v", got)
	}
	if got := splitFields("   "); len(got) != 0 {
		t.Fatalf("splitFields(blank) = %#v", got)
	}
}

func TestDNSPresetFromFlags(t *testing.T) {
	if dnsPresetFromFlags("", "", "", "") != nil {
		t.Fatal("no flags must yield no preset")
	}
	preset := dnsPresetFromFlags("split", "corp.internal, svc.cluster.local", "10.0.0.2", "10.0.0.2:53")
	if preset == nil {
		t.Fatal("flags must yield a preset")
	}
	if preset.Mode != config.DNSModeSplit {
		t.Fatalf("preset.Mode = %q", preset.Mode)
	}
	if len(preset.Domains) != 2 || preset.Domains[0] != "corp.internal" {
		t.Fatalf("preset.Domains = %#v", preset.Domains)
	}
	if len(preset.Resolver) != 1 || preset.Resolver[0] != "10.0.0.2" {
		t.Fatalf("preset.Resolver = %#v", preset.Resolver)
	}
	if preset.ToNS != "10.0.0.2:53" {
		t.Fatalf("preset.ToNS = %q", preset.ToNS)
	}
}

func TestApplyDNSFlags(t *testing.T) {
	cfg := &config.Config{}
	if err := applyDNSFlags(cfg, "split", "Corp.INTERNAL.,*.svc.cluster.local", "10.0.0.2, 10.0.0.2", "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if cfg.DNS.Mode != config.DNSModeSplit {
		t.Fatalf("mode = %q", cfg.DNS.Mode)
	}
	if len(cfg.DNS.Domains) != 2 || cfg.DNS.Domains[0] != "corp.internal" || cfg.DNS.Domains[1] != "svc.cluster.local" {
		t.Fatalf("domains = %#v", cfg.DNS.Domains)
	}
	if len(cfg.DNS.Resolver) != 1 {
		t.Fatalf("resolver = %#v", cfg.DNS.Resolver)
	}
	if cfg.DNS.ToNS != "10.0.0.2" {
		t.Fatalf("to_ns = %q", cfg.DNS.ToNS)
	}

	// Mode off must clear all the pieces.
	if err := applyDNSFlags(cfg, "off", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.DNS.Domains != nil || cfg.DNS.Resolver != nil || cfg.DNS.ToNS != "" || cfg.DNS.ManageResolver {
		t.Fatalf("off did not clear: %#v", cfg.DNS)
	}

	// Inconsistent sets must not validate.
	cfg2 := &config.Config{DNS: &config.DNSConfig{Mode: config.DNSModeSplit, Domains: []string{"corp.internal"}}}
	if err := applyDNSFlags(cfg2, "", "", "", ""); err == nil {
		t.Fatal("split mode without resolver must be rejected")
	}
}

func TestDetectVPCResolver(t *testing.T) {
	if got := detectVPCResolver([]string{"fd00:db8::/32", "10.0.0.0/16"}); got != "10.0.0.2" {
		t.Fatalf("detectVPCResolver = %q", got)
	}
	if got := detectVPCResolver([]string{"10.0.0.0/31"}); got != "" {
		t.Fatalf("tiny CIDR must not yield a resolver, got %q", got)
	}
}

func TestParseShellCommands(t *testing.T) {
	names := []string{"dns-split", "corp"}

	// Basic commands without an argument use the current connection.
	outcome, err := ui.ParseShell("stop", "corp", names)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ui.ActStop || outcome.Connection != "corp" {
		t.Fatalf("outcome = %+v", outcome)
	}

	// Explicit arguments override the current context.
	outcome, err = ui.ParseShell("start dns-split", "corp", names)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ui.ActStart || outcome.Connection != "dns-split" {
		t.Fatalf("outcome = %+v", outcome)
	}

	// The dns command takes a subcommand.
	for input, want := range map[string]string{
		"dns install": ui.ActDNSSetup,
		"dns remove":  ui.ActDNSTeardown,
		"dns edit":    ui.ActDNSEdit,
	} {
		outcome, err = ui.ParseShell(input, "corp", names)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if outcome.Action != want || outcome.Connection != "corp" {
			t.Fatalf("%s: outcome = %+v, want action %s", input, outcome, want)
		}
	}
	outcome, err = ui.ParseShell("dns install dns-split", "corp", names)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ui.ActDNSSetup || outcome.Connection != "dns-split" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if _, err = ui.ParseShell("dns", "corp", names); err == nil {
		t.Fatal("bare dns must be rejected")
	}
	if _, err = ui.ParseShell("dns paint", "corp", names); err == nil {
		t.Fatal("unknown dns subcommand must be rejected")
	}

	// Unknown names and commands, plus quit and new.
	if _, err = ui.ParseShell("stop nope", "", names); err == nil {
		t.Fatal("unknown connection must be rejected")
	}
	if _, err = ui.ParseShell("frobnicate", "", names); err == nil {
		t.Fatal("unknown command must be rejected")
	}
	outcome, err = ui.ParseShell("quit", "", names)
	if err != nil || !outcome.Quit {
		t.Fatalf("quit: outcome = %+v, err = %v", outcome, err)
	}
	outcome, err = ui.ParseShell("new", "", names)
	if err != nil || outcome.Action != ui.ActNew {
		t.Fatalf("new: outcome = %+v, err = %v", outcome, err)
	}
	outcome, err = ui.ParseShell("help", "", names)
	if err != nil || outcome.Action != "" {
		t.Fatalf("help: outcome = %+v, err = %v", outcome, err)
	}
	// A current context is required for connection commands.
	if _, err = ui.ParseShell("stop", "", names); err == nil {
		t.Fatal("missing connection must be rejected")
	}
}

func TestOfflineBlocksNetworkActions(t *testing.T) {
	if !ui.OfflineBlocks(ui.ActNew) || !ui.OfflineBlocks(ui.ActRescan) {
		t.Fatal("new and rescan must be blocked offline")
	}
	if ui.OfflineBlocks(ui.ActStop) || ui.OfflineBlocks(ui.ActStart) || ui.OfflineBlocks(ui.ActLogs) {
		t.Fatal("local tunnel actions must not be blocked offline")
	}
}
