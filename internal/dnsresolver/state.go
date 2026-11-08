// State records exactly what bastionctl touched on this machine for one
// connection's split-DNS resolver configuration. Teardown always runs from
// this snapshot — never from the current connection YAML — so later edits
// or renames can never break cleanup.
package dnsresolver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileRecord describes one darwin resolver file bastionctl placed, with the
// content hash at install time for change detection.
type FileRecord struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

// State is the on-disk manifest stored next to the connection YAML.
type State struct {
	Platform    string       `yaml:"platform"`
	Connection  string       `yaml:"connection"`
	AccountID   string       `yaml:"account_id,omitempty"`
	Domains     []string     `yaml:"domains"`
	Resolver    []string     `yaml:"resolver"`
	ToNS        string       `yaml:"to_ns,omitempty"`
	Files       []FileRecord `yaml:"files,omitempty"` // darwin
	Link        string       `yaml:"link,omitempty"`  // linux
	InstalledAt string       `yaml:"installed_at"`
}

// Describe renders the state for status output.
func (s *State) Describe() string {
	if s == nil {
		return ""
	}
	base := fmt.Sprintf("split DNS: %d domain(s) via %s", len(s.Domains), strings.Join(s.Resolver, ", "))
	switch s.Platform {
	case "darwin":
		paths := make([]string, 0, len(s.Files))
		for _, record := range s.Files {
			paths = append(paths, record.Path)
		}
		return base + " (resolver files: " + strings.Join(paths, ", ") + ")"
	case "linux":
		return base + " (routing link: " + s.Link + ")"
	default:
		return base
	}
}

// LoadState reads the manifest; a missing file yields (nil, nil).
func LoadState(statePath string) (*State, error) {
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading split-DNS state %s: %w", statePath, err)
	}
	var state State
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing split-DNS state %s: %w", statePath, err)
	}
	return &state, nil
}

func saveState(statePath string, state *State) error {
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshalling split-DNS state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return fmt.Errorf("creating split-DNS state directory: %w", err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		return fmt.Errorf("writing split-DNS state %s: %w", statePath, err)
	}
	return nil
}
