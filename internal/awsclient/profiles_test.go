package awsclient

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestListProfilesReadsNamesOnlyFromSharedFiles(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config")
	credentialsPath := filepath.Join(directory, "credentials")
	if err := os.WriteFile(configPath, []byte("[default]\nregion=eu-central-1\n[profile dev]\nrole_arn=example\n[sso-session ignored]\nsso_start_url=example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsPath, []byte("[legacy]\naws_access_key_id=not-a-real-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsPath)

	profiles, err := ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "dev", "legacy"}
	if !reflect.DeepEqual(profiles, want) {
		t.Fatalf("ListProfiles() = %v, want %v", profiles, want)
	}
}
