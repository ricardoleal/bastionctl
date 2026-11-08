package config

import (
	"reflect"
	"strings"
	"testing"
)

func testSplitDNS() *DNSConfig {
	return &DNSConfig{
		Mode:     DNSModeSplit,
		Resolver: []string{"10.0.0.2"},
		Domains:  []string{"corp.internal", "svc.cluster.local"},
	}
}

func TestEffectiveModeNilMeansOff(t *testing.T) {
	var nilDNS *DNSConfig
	if nilDNS.EffectiveMode() != DNSModeOff {
		t.Fatal("nil DNS must mean off")
	}
	if (&DNSConfig{Mode: DNSModeSplit}).EffectiveMode() != DNSModeSplit {
		t.Fatal("blank mode should not override set mode")
	}
	if (&DNSConfig{}).EffectiveMode() != DNSModeOff {
		t.Fatal("blank mode must mean off")
	}
}

func TestNormalizeOffClearsEverything(t *testing.T) {
	d := &DNSConfig{
		Mode:           DNSModeOff,
		Resolver:       []string{"10.0.0.2"},
		Domains:        []string{"corp.internal"},
		ToNS:           "10.0.0.2",
		ManageResolver: true,
	}
	if err := d.Normalize(); err != nil {
		t.Fatal(err)
	}
	if d.Domains != nil || d.Resolver != nil || d.ToNS != "" || d.ManageResolver {
		t.Fatalf("off mode left fields set: %#v", d)
	}
}

func TestNormalizeAllClearsDomainsAndResolver(t *testing.T) {
	d := &DNSConfig{Mode: DNSModeAll, Resolver: []string{"10.0.0.2"}, Domains: []string{"corp.internal"}, ToNS: "10.0.9.9"}
	if err := d.Normalize(); err != nil {
		t.Fatal(err)
	}
	if d.Domains != nil || d.Resolver != nil {
		t.Fatalf("all mode must drop domains/resolver: %#v", d)
	}
	if d.ToNS != "10.0.9.9" {
		t.Fatalf("all mode must keep to_ns, got %q", d.ToNS)
	}
}

func TestNormalizeAllUsesResolverAsForwardTarget(t *testing.T) {
	dns := &DNSConfig{Mode: DNSModeAll, Resolver: []string{"172.31.0.2"}}
	if err := dns.Normalize(); err != nil {
		t.Fatal(err)
	}
	if dns.ToNS != "172.31.0.2" || len(dns.Resolver) != 0 {
		t.Fatalf("normalized full DNS = %+v", dns)
	}
}

func TestEffectiveDNSConfigDefaultsFullModeToVPCResolver(t *testing.T) {
	cfg := &Config{
		VpcCIDRs: []string{"172.31.0.0/16"},
		DNS:      &DNSConfig{Mode: DNSModeAll},
	}
	effective := cfg.EffectiveDNSConfig()
	if effective.ToNS != "172.31.0.2" {
		t.Fatalf("effective ToNS = %q, want VPC resolver", effective.ToNS)
	}
	if cfg.DNS.ToNS != "" {
		t.Fatalf("effective config mutated saved ToNS: %q", cfg.DNS.ToNS)
	}
	cfg.DNS.Resolver = []string{"10.20.0.53"}
	if got := cfg.EffectiveDNSConfig().ToNS; got != "10.20.0.53" {
		t.Fatalf("configured resolver forward target = %q", got)
	}
	cfg.DNS.Resolver = nil
	cfg.DNS.ToNS = "10.20.0.53"
	if got := cfg.EffectiveDNSConfig().ToNS; got != "10.20.0.53" {
		t.Fatalf("explicit ToNS = %q, want 10.20.0.53", got)
	}
}

func TestNormalizeSplitRequirements(t *testing.T) {
	d := &DNSConfig{Mode: DNSModeSplit}
	if err := d.Normalize(); err == nil || !strings.Contains(err.Error(), "resolver") {
		t.Fatalf("Normalize() = %v, want resolver requirement", err)
	}
	d.Resolver = []string{"10.0.0.2"}
	if err := d.Normalize(); err == nil || !strings.Contains(err.Error(), "domain") {
		t.Fatalf("Normalize() = %v, want domain requirement", err)
	}
	d.Domains = []string{"corp.internal"}
	if err := d.Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeDomains(t *testing.T) {
	got, err := NormalizeDomains([]string{" Corp.INTERNAL.", "*.Corp.Internal", "svc.cluster.local", " corp.internal "})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"corp.internal", "svc.cluster.local"}) {
		t.Fatalf("NormalizeDomains = %#v", got)
	}

	for _, bad := range []string{"", "/", "a b", "-x.y", "x..y", "corp\\internal"} {
		if _, err := NormalizeDomains([]string{bad}); err == nil {
			t.Fatalf("accepted invalid domain %q", bad)
		}
	}
}

func TestNormalizeResolverAddrs(t *testing.T) {
	got, err := NormalizeResolverAddrs([]string{"10.0.0.2", "10.0.0.2", "fd00:ec2::253"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"10.0.0.2", "fd00:ec2::253"}) {
		t.Fatalf("NormalizeResolverAddrs = %#v", got)
	}

	for _, bad := range []string{"10.0.0.2:53", "corp.internal", "fe80::1%eth0", ""} {
		if _, err := NormalizeResolverAddrs([]string{bad}); err == nil {
			t.Fatalf("accepted invalid resolver %q", bad)
		}
	}
}

func TestNormalizeToNS(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  10.0.0.2  ", "10.0.0.2"},
		{"10.0.0.2:5353", "10.0.0.2:5353"},
		{"fd00:ec2::253", "fd00:ec2::253"},
		{"[fd00:ec2::253]:5353", "[fd00:ec2::253]:5353"},
	}
	for _, tc := range cases {
		got, err := NormalizeToNS(tc.in)
		if err != nil {
			t.Fatalf("NormalizeToNS(%q) error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeToNS(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"10.0.0.2:0", "corp.internal:53", "10.0.0.2:99999", "10.0.0.2:notaport"} {
		if _, err := NormalizeToNS(bad); err == nil {
			t.Fatalf("accepted invalid to_ns %q", bad)
		}
	}
}

func TestNormalizeRejectsUnknownMode(t *testing.T) {
	d := &DNSConfig{Mode: "sometimes"}
	if err := d.Normalize(); err == nil || !strings.Contains(err.Error(), "invalid DNS mode") {
		t.Fatalf("Normalize() = %v", err)
	}
}

func TestStoreRoundTripsAndNormalizesDNS(t *testing.T) {
	store := NewStore(t.TempDir())
	want := &Config{
		Kind:       ProfileKindVPC,
		Region:     "eu-central-1",
		Target:     "i-123",
		InstanceID: "i-123",
		SSHPort:    22,
		VpcCIDRs:   []string{"10.0.0.0/16"},
		DNS: &DNSConfig{
			Mode:           DNSModeSplit,
			Resolver:       []string{"10.0.0.2"},
			Domains:        []string{"*.Corp.Internal."},
			ManageResolver: true,
		},
	}
	if err := store.Save("111111111111", "corp", want); err != nil {
		t.Fatal(err)
	}
	if want.DNS.Domains[0] != "corp.internal" {
		t.Fatalf("Save did not normalize domains: %#v", want.DNS.Domains)
	}

	loaded, err := store.Load("111111111111", "corp")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DNS == nil || loaded.DNS.Mode != DNSModeSplit || loaded.DNS.Domains[0] != "corp.internal" {
		t.Fatalf("round-tripped DNS = %#v", loaded.DNS)
	}
	if !loaded.DNS.ManageResolver {
		t.Fatal("manage_resolver did not round-trip")
	}

	// A split config missing pieces must be rejected by Save.
	if err := store.Save("111111111111", "broken", &Config{
		Region:     "eu-central-1",
		Target:     "i-1",
		InstanceID: "i-1",
		DNS:        &DNSConfig{Mode: DNSModeSplit, Domains: []string{"corp.internal"}},
	}); err == nil {
		t.Fatal("Save accepted a split config without a resolver")
	}
}

func TestSummary(t *testing.T) {
	if got := testSplitDNS().Summary(); got != "split (2 domain(s) via 10.0.0.2)" {
		t.Fatalf("Summary() = %q", got)
	}
	if got := (&DNSConfig{Mode: DNSModeAll}).Summary(); got != "all (--dns)" {
		t.Fatalf("Summary() = %q", got)
	}
	if got := (&DNSConfig{Mode: DNSModeAll, ToNS: "10.0.9.9"}).Summary(); got != "all (--dns, forwarding to 10.0.9.9)" {
		t.Fatalf("Summary() = %q", got)
	}
	var nilDNS *DNSConfig
	if got := nilDNS.Summary(); got != "off" {
		t.Fatalf("nil Summary() = %q", got)
	}
}
