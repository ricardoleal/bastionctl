package netcidr

import (
	"net/netip"
	"testing"
)

func TestSecondIPv4Host(t *testing.T) {
	cases := []struct {
		cidr string
		want string
	}{
		{"10.0.0.0/16", "10.0.0.2"},
		{"172.16.5.0/24", "172.16.5.2"},
		{"192.168.1.48/28", "192.168.1.50"},
		{"10.0.0.0/8", "10.0.0.2"},
	}
	for _, tc := range cases {
		got, err := SecondIPv4Host(tc.cidr)
		if err != nil {
			t.Fatalf("SecondIPv4Host(%q) error: %v", tc.cidr, err)
		}
		if got.String() != tc.want {
			t.Fatalf("SecondIPv4Host(%q) = %s, want %s", tc.cidr, got, tc.want)
		}
	}

	for _, cidr := range []string{"10.0.0.0/31", "10.0.0.0/32", "fd00::/64", "2001:db8::/32"} {
		if _, err := SecondIPv4Host(cidr); err == nil {
			t.Fatalf("SecondIPv4Host(%q) unexpectedly succeeded", cidr)
		}
	}
	if _, err := SecondIPv4Host("not-a-cidr"); err == nil {
		t.Fatal("SecondIPv4Host accepted garbage")
	}
}

func TestContains(t *testing.T) {
	addr := netip.MustParseAddr("10.0.0.2")
	if !Contains([]string{"10.0.0.0/16"}, addr) {
		t.Fatal("exact prefix should contain the address")
	}
	if !Contains([]string{"192.0.2.0/24", "10.0.0.0/16"}, addr) {
		t.Fatal("any-match should contain the address")
	}
	if Contains([]string{"192.0.2.0/24"}, addr) {
		t.Fatal("non-matching prefix must not contain the address")
	}
	if Contains([]string{"bogus-cidr"}, addr) {
		t.Fatal("unparseable CIDR must be skipped, not match")
	}
	v6 := netip.MustParseAddr("fd00::1")
	if Contains([]string{"10.0.0.0/16"}, v6) {
		t.Fatal("v4 prefix must not contain a v6 address")
	}
	if Contains(nil, netip.Addr{}) {
		t.Fatal("invalid address must not match anything")
	}
}
