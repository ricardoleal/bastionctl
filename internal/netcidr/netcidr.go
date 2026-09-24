// Package netcidr provides small CIDR-containment helpers, used to
// determine whether a Security Group rule's CIDR fully covers a given
// target CIDR block (e.g. checking that an instance's egress rules allow
// it to reach its VPC's own address range).
package netcidr

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
)

// Covers reports whether ruleCIDR fully contains targetCIDR -- i.e.
// ruleCIDR is equal to, or a supernet of, targetCIDR. Returns false if
// either CIDR fails to parse or they are different IP address families.
func Covers(ruleCIDR, targetCIDR string) bool {
	_, ruleNet, err := net.ParseCIDR(ruleCIDR)
	if err != nil {
		return false
	}
	_, targetNet, err := net.ParseCIDR(targetCIDR)
	if err != nil {
		return false
	}

	ruleOnes, ruleBits := ruleNet.Mask.Size()
	targetOnes, targetBits := targetNet.Mask.Size()
	if ruleBits != targetBits {
		return false // different address families (v4 vs v6)
	}
	if ruleOnes > targetOnes {
		return false // rule's range is narrower than the target; can't fully cover it
	}
	return ruleNet.Contains(targetNet.IP)
}

// CoversAny reports whether any CIDR in ruleCIDRs covers targetCIDR.
func CoversAny(ruleCIDRs []string, targetCIDR string) bool {
	for _, r := range ruleCIDRs {
		if Covers(r, targetCIDR) {
			return true
		}
	}
	return false
}

func Normalize(cidr string) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	return prefix.Masked().String(), nil
}

func NormalizeUnique(cidrs []string) ([]string, error) {
	seen := make(map[string]struct{}, len(cidrs))
	result := make([]string, 0, len(cidrs))
	for _, cidr := range cidrs {
		normalized, err := Normalize(cidr)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result, nil
}
