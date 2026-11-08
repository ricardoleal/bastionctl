// Package config handles loading/saving the bastionctl YAML config file
// that persists the chosen sshuttle pivot target between runs.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"bastionctl/internal/netcidr"

	"gopkg.in/yaml.v3"
)

const appName = "bastionctl"

type ProfileKind string

const (
	ProfileKindVPC       ProfileKind = "vpc"
	ProfileKindDedicated ProfileKind = "dedicated"
)

type NetworkRoute struct {
	CIDR        string `yaml:"cidr"`
	Source      string `yaml:"source"`
	TargetType  string `yaml:"target_type,omitempty"`
	TargetID    string `yaml:"target_id,omitempty"`
	Description string `yaml:"description,omitempty"`
	Selected    bool   `yaml:"selected"`
}

// DNSMode selects how bastionctl wires DNS into sshuttle.
//
// off: no DNS flags at all (tunnel routes subnets only; the historical
//
//	behavior).
//
// all: sshuttle --dns captures every query sent to the local system
//
//	resolvers and resolves them on the remote side of the tunnel.
//
// split: sshuttle --ns-hosts captures only queries sent to the configured
//
//	resolver IPs; the OS resolver is expected to send only the listed
//	domain suffixes to those IPs (see internal/dnsresolver).
type DNSMode string

const (
	DNSModeOff   DNSMode = "off"
	DNSModeAll   DNSMode = "all"
	DNSModeSplit DNSMode = "split"
)

// dnsDomainPattern validates a plain DNS suffix: lowercase letters, digits,
// dashes, dots. It never accepts slashes, characters that would break the
// macOS /etc/resolver/<domain> filename, or wildcards (suffix routing already
// covers subdomains, so "*.corp.internal" is normalized to "corp.internal").
var dnsDomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// DNSConfig holds a connection's DNS wiring. Resolver is the *intercept*
// target list (passed to sshuttle --ns-hosts: queries directed at these IPs
// are captured and tunneled); ToNS is the *forward* target the remote side
// resolves with (sshuttle --to-ns). Those are different roles even though
// they usually hold the same VPC resolver IP.
type DNSConfig struct {
	Mode           DNSMode  `yaml:"mode,omitempty"`
	Resolver       []string `yaml:"resolver,omitempty"`
	Domains        []string `yaml:"domains,omitempty"`
	ToNS           string   `yaml:"to_ns,omitempty"`
	ManageResolver bool     `yaml:"manage_resolver,omitempty"`
}

// EffectiveMode treats a missing DNS block (or blank mode) as off.
func (d *DNSConfig) EffectiveMode() DNSMode {
	if d == nil || d.Mode == "" {
		return DNSModeOff
	}
	return d.Mode
}

// ParseDNSMode validates a user-provided DNS mode string.
func ParseDNSMode(s string) (DNSMode, error) {
	switch DNSMode(s) {
	case DNSModeOff, DNSModeAll, DNSModeSplit:
		return DNSMode(s), nil
	default:
		return "", fmt.Errorf("invalid DNS mode %q (use off, all, or split)", s)
	}
}

// Summary renders a human-readable one-liner for status output.
func (d *DNSConfig) Summary() string {
	mode := d.EffectiveMode()
	switch mode {
	case DNSModeAll:
		if d != nil && d.ToNS != "" {
			return fmt.Sprintf("all (--dns, forwarding to %s)", d.ToNS)
		}
		return "all (--dns)"
	case DNSModeSplit:
		return fmt.Sprintf("split (%d domain(s) via %s)", len(d.Domains), strings.Join(d.Resolver, ", "))
	default:
		return "off"
	}
}

// NormalizeDomains lowercases, trims trailing dots, strips a leading "*.",
// validates each entry, dedupes while preserving order. An empty input is
// allowed (mode-specific requirements are enforced by DNSConfig.Normalize).
func NormalizeDomains(domains []string) ([]string, error) {
	out := make([]string, 0, len(domains))
	seen := make(map[string]struct{}, len(domains))
	for _, raw := range domains {
		s := strings.ToLower(strings.TrimSpace(raw))
		s = strings.TrimSuffix(s, ".")
		s = strings.TrimPrefix(s, "*.")
		if s == "" {
			return nil, errors.New("split DNS domain list contains an empty entry")
		}
		if len(s) > 253 {
			return nil, fmt.Errorf("split DNS domain %q exceeds 253 bytes", s)
		}
		if !dnsDomainPattern.MatchString(s) {
			return nil, fmt.Errorf("invalid split DNS domain %q (use plain domain suffixes like corp.internal)", s)
		}
		if _, exists := seen[s]; exists {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out, nil
}

// NormalizeResolverAddrs validates and dedupes resolver IP addresses
// (netip.ParseAddr, no zones, no ports) preserving order.
func NormalizeResolverAddrs(addrs []string) ([]string, error) {
	out := make([]string, 0, len(addrs))
	seen := make(map[string]struct{}, len(addrs))
	for _, raw := range addrs {
		s := strings.TrimSpace(raw)
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("invalid resolver IP %q (%v)", raw, err)
		}
		if addr.Zone() != "" {
			return nil, fmt.Errorf("resolver IP %q must not carry a zone", raw)
		}
		if _, exists := seen[addr.String()]; exists {
			continue
		}
		seen[addr.String()] = struct{}{}
		out = append(out, addr.String())
	}
	return out, nil
}

// NormalizeToNS validates an sshuttle --to-ns target ("IP" or "IP:port";
// bracketed "[%s]:port" form for IPv6) and returns its canonical text.
func NormalizeToNS(v string) (string, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", nil
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		if addr.Zone() != "" {
			return "", fmt.Errorf("forward target %q must not carry a zone", v)
		}
		return addr.String(), nil
	}
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("invalid forward target %q (use IP or IP:port)", v)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("invalid forward target %q (%v)", v, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid forward target %q (port must be 1-65535)", v)
	}
	if addr.Is6() {
		return "[" + addr.String() + "]:" + portText, nil
	}
	return addr.String() + ":" + portText, nil
}

// Normalize validates and normalizes the DNS settings in place, enforcing
// mode-specific requirements:
//
//   - off: clears domains/resolver/to_ns and manage_resolver.
//   - all: clears domains and resolver (both unused for --dns); keeps to_ns.
//   - split: requires at least one domain and one resolver IP.
func (d *DNSConfig) Normalize() error {
	if d.Mode == "" {
		d.Mode = DNSModeOff
	}
	switch d.Mode {
	case DNSModeOff, DNSModeAll, DNSModeSplit:
	default:
		return fmt.Errorf("invalid DNS mode %q (use off, all, or split)", d.Mode)
	}

	toNSText := d.ToNS
	if d.Mode == DNSModeAll && strings.TrimSpace(toNSText) == "" && len(d.Resolver) > 0 {
		toNSText = d.Resolver[0]
	}
	toNS, err := NormalizeToNS(toNSText)
	if err != nil {
		return err
	}
	d.ToNS = toNS

	switch d.Mode {
	case DNSModeOff:
		d.Domains, d.Resolver, d.ManageResolver, d.ToNS = nil, nil, false, ""
	case DNSModeAll:
		d.Domains, d.Resolver = nil, nil
	case DNSModeSplit:
		if len(d.Resolver) == 0 {
			return errors.New("split DNS requires at least one resolver IP (--dns-resolver)")
		}
		resolver, err := NormalizeResolverAddrs(d.Resolver)
		if err != nil {
			return err
		}
		d.Resolver = resolver
		if len(d.Domains) == 0 {
			return errors.New("split DNS requires at least one domain suffix (--dns-domain)")
		}
		domains, err := NormalizeDomains(d.Domains)
		if err != nil {
			return err
		}
		d.Domains = domains
	}
	return nil
}

// Config is the persisted pivot target and connection details.
type Config struct {
	Name       string         `yaml:"name,omitempty"`
	Kind       ProfileKind    `yaml:"kind,omitempty"`
	AccountID  string         `yaml:"account_id"`
	AWSProfile string         `yaml:"aws_profile,omitempty"`
	Region     string         `yaml:"region"`
	VpcID      string         `yaml:"vpc_id"`
	VpcCIDRs   []string       `yaml:"vpc_cidrs"`
	SubnetID   string         `yaml:"subnet_id,omitempty"`
	InstanceID string         `yaml:"instance_id"`
	NameTag    string         `yaml:"name_tag"`
	Routes     []NetworkRoute `yaml:"routes,omitempty"`

	// ConnectionMethod is "ssm" or "direct_ip". Target is the ssh host
	// argument to use: an instance ID for "ssm" (matched by a Host
	// pattern such as `Host i-* mi-*` in ~/.ssh/config that proxies
	// through AWS Systems Manager Session Manager), or an IP address for
	// "direct_ip".
	ConnectionMethod string `yaml:"connection_method"`
	Target           string `yaml:"target"`

	// SSHUser/SSHKeyPath are left blank by default for "ssm" targets so
	// whatever User/IdentityAgent is already configured in ~/.ssh/config
	// keeps being used untouched. For "direct_ip" targets they are
	// normally required.
	SSHUser    string `yaml:"ssh_user,omitempty"`
	SSHKeyPath string `yaml:"ssh_key_path,omitempty"`
	SSHPort    int    `yaml:"ssh_port"`

	// DNS wires resolver behavior into sshuttle; nil means mode "off"
	// (no DNS flags, the historical behavior).
	DNS *DNSConfig `yaml:"dns,omitempty"`

	SavedAt    string `yaml:"saved_at"`
	UpdatedAt  string `yaml:"updated_at,omitempty"`
	LastUsedAt string `yaml:"last_used_at,omitempty"`
}

func (c *Config) SelectedCIDRs() []string {
	if len(c.Routes) == 0 {
		return append([]string(nil), c.VpcCIDRs...)
	}
	var cidrs []string
	for _, route := range c.Routes {
		if route.Selected {
			cidrs = append(cidrs, route.CIDR)
		}
	}
	return cidrs
}

// DefaultDNSResolver returns the AWS VPC resolver address derived from the
// connection's first IPv4 VPC CIDR, when one is available.
func (c *Config) DefaultDNSResolver() string {
	if c == nil {
		return ""
	}
	for _, cidr := range c.VpcCIDRs {
		if addr, err := netcidr.SecondIPv4Host(cidr); err == nil {
			return addr.String()
		}
	}
	return ""
}

// EffectiveDNSConfig returns a copy with the VPC resolver as the default
// forward target for full DNS mode. It never mutates the saved config.
func (c *Config) EffectiveDNSConfig() *DNSConfig {
	if c == nil || c.DNS == nil {
		return nil
	}
	effective := *c.DNS
	if effective.EffectiveMode() == DNSModeAll && effective.ToNS == "" {
		if len(effective.Resolver) > 0 {
			effective.ToNS = effective.Resolver[0]
		} else {
			effective.ToNS = c.DefaultDNSResolver()
		}
	}
	return &effective
}

// DNSSummary reports the configured DNS behavior including inferred full-mode
// forwarding to the VPC resolver.
func (c *Config) DNSSummary() string {
	if c == nil || c.DNS == nil {
		return "off"
	}
	return c.EffectiveDNSConfig().Summary()
}

// DefaultPath resolves ~/.config/bastionctl/config.yaml, honoring
// $XDG_CONFIG_HOME if set.
func DefaultPath() (string, error) {
	root, err := DefaultRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "config.yaml"), nil
}

func DefaultRoot() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".config", appName), nil
}

// Load reads and parses the config file at path. Returns an error
// (including os.ErrNotExist wrapped) if the file is missing or invalid.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes cfg to path as YAML, creating parent directories as needed.
// The file is written with 0600 permissions since it may reference a
// private key path.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing config %s: %w", path, err)
	}
	return nil
}

// Forget deletes the saved config file.
func Forget(path string) error {
	return os.Remove(path)
}

type Store struct {
	Root string
}

func NewStore(root string) *Store {
	return &Store{Root: root}
}

var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func ValidateProfileName(name string) error {
	if name == "" || name == "." || name == ".." || !profileNamePattern.MatchString(name) {
		return fmt.Errorf("invalid connection name %q; use letters, numbers, dots, dashes, or underscores", name)
	}
	return nil
}

func (s *Store) ProfilePath(accountID, name string) (string, error) {
	if strings.TrimSpace(accountID) == "" || strings.ContainsAny(accountID, `/\\`) {
		return "", fmt.Errorf("invalid AWS account ID %q", accountID)
	}
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.Root, "accounts", accountID, name+".yaml"), nil
}

func (s *Store) List(accountID string) ([]*Config, error) {
	dir := filepath.Join(s.Root, "accounts", accountID)
	entries, err := os.ReadDir(dir)
	if errorsIsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing connections for account %s: %w", accountID, err)
	}
	var profiles []*Config
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		profile, err := s.Load(accountID, name)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func (s *Store) Load(accountID, name string) (*Config, error) {
	path, err := s.ProfilePath(accountID, name)
	if err != nil {
		return nil, err
	}
	profile, err := Load(path)
	if err != nil {
		return nil, err
	}
	if profile.AccountID != accountID {
		return nil, fmt.Errorf("connection %s belongs to account %s, not %s", name, profile.AccountID, accountID)
	}
	if profile.Name == "" {
		profile.Name = name
	}
	return profile, nil
}

func (s *Store) Save(accountID, name string, cfg *Config) error {
	path, err := s.ProfilePath(accountID, name)
	if err != nil {
		return err
	}
	if cfg.AccountID != "" && cfg.AccountID != accountID {
		return fmt.Errorf("connection account %s does not match active account %s", cfg.AccountID, accountID)
	}
	cfg.AccountID = accountID
	cfg.Name = name
	if cfg.Kind == "" {
		cfg.Kind = ProfileKindVPC
	}
	for index := range cfg.Routes {
		cidr, err := netcidr.Normalize(cfg.Routes[index].CIDR)
		if err != nil {
			return fmt.Errorf("connection %s route: %w", name, err)
		}
		cfg.Routes[index].CIDR = cidr
	}
	if cfg.DNS != nil {
		if err := cfg.DNS.Normalize(); err != nil {
			return fmt.Errorf("connection %s DNS settings: %w", name, err)
		}
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling connection %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating account config directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".connection-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary connection file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing connection %s: %w", name, err)
	}
	return nil
}

func (s *Store) Delete(accountID, name string) error {
	path, err := s.ProfilePath(accountID, name)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *Store) MigrateLegacy() (string, bool, error) {
	legacyPath := filepath.Join(s.Root, "config.yaml")
	legacy, err := Load(legacyPath)
	if errorsIsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("loading legacy config: %w", err)
	}
	name := "default"
	for suffix := 0; ; suffix++ {
		if suffix == 1 {
			name = "legacy"
		} else if suffix > 1 {
			name = fmt.Sprintf("legacy-%d", suffix)
		}
		if _, err := s.Load(legacy.AccountID, name); errorsIsNotExist(err) {
			break
		} else if err != nil {
			return "", false, err
		}
	}
	legacy.Kind = ProfileKindVPC
	if len(legacy.Routes) == 0 {
		for _, cidr := range legacy.VpcCIDRs {
			legacy.Routes = append(legacy.Routes, NetworkRoute{CIDR: cidr, Source: "pivot_vpc", Selected: true})
		}
	}
	if err := s.Save(legacy.AccountID, name, legacy); err != nil {
		return "", false, err
	}
	if err := os.Rename(legacyPath, legacyPath+".migrated"); err != nil {
		return "", false, fmt.Errorf("backing up legacy config: %w", err)
	}
	return name, true, nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}
