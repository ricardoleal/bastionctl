package awsclient

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ListProfiles returns profile names found in the AWS shared config and
// credentials files without loading or exposing credential values.
func ListProfiles() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	configPath := os.Getenv("AWS_CONFIG_FILE")
	if configPath == "" {
		configPath = filepath.Join(home, ".aws", "config")
	}
	credentialsPath := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credentialsPath == "" {
		credentialsPath = filepath.Join(home, ".aws", "credentials")
	}
	profiles := map[string]struct{}{"default": {}}
	for _, path := range []string{configPath, credentialsPath} {
		file, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
				continue
			}
			section := strings.TrimSpace(line[1 : len(line)-1])
			if section == "default" {
				profiles[section] = struct{}{}
			} else if strings.HasPrefix(section, "profile ") {
				name := strings.TrimSpace(strings.TrimPrefix(section, "profile "))
				if name != "" {
					profiles[name] = struct{}{}
				}
			} else if !strings.Contains(section, " ") && section != "sso-session" {
				profiles[section] = struct{}{}
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	result := make([]string, 0, len(profiles))
	for profile := range profiles {
		result = append(result, profile)
	}
	sort.Strings(result)
	return result, nil
}
