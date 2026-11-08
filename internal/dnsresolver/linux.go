package dnsresolver

import (
	"fmt"
	"strings"

	"bastionctl/internal/config"
)

// linuxLinkName derives the deterministic dummy-link name for a connection:
// "bctl-" plus the first 8 hex chars of sha256(connection), always ≤15 chars
// (Linux interface name limit).
func linuxLinkName(connection string) string {
	return "bctl-" + sha256Hex([]byte(connection))[:8]
}

// linuxLinkNotFound matches `ip link show` stderr when the interface is
// absent.
func linuxLinkNotFound(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "Cannot find device") || strings.Contains(s, "No such device")
}

// installLinux routes the split domains through a dedicated dummy link
// registered with systemd-resolved:
//
//	ip link add <link> type dummy
//	ip link set <link> up
//	resolvectl dns <link> <resolver IPs...>
//	resolvectl domain <link> ~<domain>...
//
// The ~ prefix makes each domain a routing domain: systemd-resolved sends
// only queries matching that suffix to this link's servers; everything else
// keeps using the system's regular resolvers. A crashed run is reclaimed
// idempotently as long as the existing link is one of bastionctl's dummies
// (same deterministic name).
func (o *Options) installLinux(cfg *config.Config, d *config.DNSConfig, existing *State) (*State, error) {
	if err := o.checkLinuxPrereqs(d); err != nil {
		return nil, err
	}
	link := linuxLinkName(cfg.Name)

	out, err := o.run("ip", "link", "show", link)
	created := false
	if err != nil {
		if !linuxLinkNotFound(out) {
			return nil, err
		}
		if _, err := o.runSudo("ip", "link", "add", link, "type", "dummy"); err != nil {
			return nil, err
		}
		created = true
	} else if !strings.Contains(string(out), "dummy") {
		return nil, fmt.Errorf("network interface %s already exists and is not a bastionctl dummy link; refusing to reuse it", link)
	}

	fail := func(cause error) error {
		if created {
			// Roll back the link we just created.
			_, _ = o.runSudo("ip", "link", "del", link)
		}
		return cause
	}

	if _, err := o.runSudo("ip", "link", "set", link, "up"); err != nil {
		return nil, fail(err)
	}
	dnsArgs := append([]string{"resolvectl", "dns", link}, d.Resolver...)
	if _, err := o.runSudo(dnsArgs[0], dnsArgs[1:]...); err != nil {
		return nil, fail(err)
	}
	domainArgs := make([]string, 0, len(d.Domains)+3)
	domainArgs = append(domainArgs, "resolvectl", "domain", link)
	for _, domain := range d.Domains {
		domainArgs = append(domainArgs, "~"+domain)
	}
	if _, err := o.runSudo(domainArgs[0], domainArgs[1:]...); err != nil {
		return nil, fail(err)
	}
	return &State{Link: link}, nil
}

// checkLinuxPrereqs verifies the toolchain up front so a half-configured
// system fails with an actionable message (and the dnsmasq fallback) before
// any system state is touched.
func (o *Options) checkLinuxPrereqs(d *config.DNSConfig) error {
	if o.LookPath != nil {
		if _, err := o.LookPath("ip"); err != nil {
			return fmt.Errorf("split DNS on Linux requires the 'ip' tool (iproute2): %w", err)
		}
		if _, err := o.LookPath("resolvectl"); err != nil {
			return fmt.Errorf("split DNS on Linux requires resolvectl (systemd-resolved); alternatively configure dnsmasq: %s", dnsmasqSnippet(d))
		}
	}
	if _, err := o.Run("resolvectl", "status"); err != nil {
		return fmt.Errorf("systemd-resolved is not running; split DNS needs it, or configure dnsmasq: %s", dnsmasqSnippet(d))
	}
	return nil
}

// dnsmasqSnippet renders the fallback configuration for systems without
// systemd-resolved.
func dnsmasqSnippet(d *config.DNSConfig) string {
	parts := make([]string, 0, len(d.Domains))
	for _, domain := range d.Domains {
		for _, ip := range d.Resolver {
			parts = append(parts, fmt.Sprintf("server=/%s/%s", domain, ip))
		}
	}
	return strings.Join(parts, " ")
}

func (o *Options) removeLinux(state *State) ([]string, error) {
	if state.Link == "" {
		return nil, nil
	}
	out, err := o.runSudo("ip", "link", "del", state.Link)
	if err != nil {
		if linuxLinkNotFound(out) {
			return nil, nil // already gone; treat as success.
		}
		return nil, err
	}
	return nil, nil
}

// linuxSetupInstructions renders the copy-paste commands that replicate the
// managed install for the operator who runs bastionctl without --manage-dns.
func (o *Options) linuxSetupInstructions(name string, d *config.DNSConfig) string {
	sudo := o.sudoHint()
	link := linuxLinkName(name)
	var b strings.Builder
	fmt.Fprintf(&b, "%sip link add %s type dummy\n", sudo, link)
	fmt.Fprintf(&b, "%sip link set %s up\n", sudo, link)
	fmt.Fprintf(&b, "%sresolvectl dns %s %s\n", sudo, link, strings.Join(d.Resolver, " "))
	quoted := make([]string, 0, len(d.Domains))
	for _, domain := range d.Domains {
		quoted = append(quoted, fmt.Sprintf("'~%s'", domain))
	}
	fmt.Fprintf(&b, "%sresolvectl domain %s %s\n", sudo, link, strings.Join(quoted, " "))
	return b.String()
}

func (o *Options) linuxTeardownInstructions(name string) string {
	sudo := o.sudoHint()
	return fmt.Sprintf("%sip link del %s\n", sudo, linuxLinkName(name))
}
