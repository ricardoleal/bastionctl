package main

import "testing"

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
