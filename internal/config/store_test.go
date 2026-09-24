package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreSeparatesAccountsAndRoundTripsRoutes(t *testing.T) {
	store := NewStore(t.TempDir())
	want := &Config{
		Kind:       ProfileKindDedicated,
		Region:     "eu-central-1",
		Target:     "i-123",
		VpcCIDRs:   []string{"10.0.0.0/16"},
		Routes:     []NetworkRoute{{CIDR: "10.20.0.0/16", Source: "manual", Selected: true}},
		SSHPort:    22,
		InstanceID: "i-123",
	}
	if err := store.Save("111111111111", "prod", want); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("222222222222", "other", &Config{Target: "i-456"}); err != nil {
		t.Fatal(err)
	}

	profiles, err := store.List("111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Name != "prod" {
		t.Fatalf("profiles = %#v", profiles)
	}
	if !reflect.DeepEqual(profiles[0].SelectedCIDRs(), []string{"10.20.0.0/16"}) {
		t.Fatalf("selected CIDRs = %v", profiles[0].SelectedCIDRs())
	}
	path, _ := store.ProfilePath("111111111111", "prod")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestStoreRejectsUnsafeNames(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, name := range []string{"", "..", "a/b", "a b"} {
		if err := store.Save("111111111111", name, &Config{}); err == nil {
			t.Fatalf("Save accepted unsafe name %q", name)
		}
	}
}

func TestStoreRejectsInvalidRoute(t *testing.T) {
	store := NewStore(t.TempDir())
	err := store.Save("111111111111", "invalid", &Config{
		Routes: []NetworkRoute{{CIDR: "not-a-cidr", Selected: true}},
	})
	if err == nil {
		t.Fatal("Save accepted an invalid route")
	}
}

func TestMigrateLegacyIsIdempotent(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	legacy := &Config{AccountID: "111111111111", Target: "i-123", VpcCIDRs: []string{"10.0.0.0/16"}}
	if err := Save(filepath.Join(root, "config.yaml"), legacy); err != nil {
		t.Fatal(err)
	}

	name, migrated, err := store.MigrateLegacy()
	if err != nil {
		t.Fatal(err)
	}
	if !migrated || name != "default" {
		t.Fatalf("migration = %q, %v", name, migrated)
	}
	profile, err := store.Load("111111111111", "default")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(profile.SelectedCIDRs(), []string{"10.0.0.0/16"}) {
		t.Fatalf("selected CIDRs = %v", profile.SelectedCIDRs())
	}
	if _, err := os.Stat(filepath.Join(root, "config.yaml.migrated")); err != nil {
		t.Fatal(err)
	}
	if _, migrated, err := store.MigrateLegacy(); err != nil || migrated {
		t.Fatalf("second migration = %v, %v", migrated, err)
	}
}
