// Package config handles loading/saving the bastionctl YAML config file
// that persists the chosen sshuttle pivot target between runs.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
