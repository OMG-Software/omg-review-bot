// Package netguard keeps requests to addresses an organization chose (its own
// model's server) on the public internet, so they can't reach the bot's own
// host or network: loopback, private, link-local and other special ranges.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// special lists ranges that aren't public unicast and that the netip
// predicates below don't cover. They're the IANA special-purpose registries'
// ranges (RFC 6890 and successors), listed to be refused: no connection is
// ever made to them, hence the NOSONAR on each (Sonar's "hardcoded IP" rule;
// it must start the comment to count).
var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // NOSONAR: "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // NOSONAR: carrier-grade NAT (shared address space)
	netip.MustParsePrefix("192.0.0.0/24"),    // NOSONAR: IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // NOSONAR: documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // NOSONAR: benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // NOSONAR: documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // NOSONAR: documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // NOSONAR: reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NOSONAR: NAT64: can map to any IPv4 address, private ones included
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NOSONAR: local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // NOSONAR: documentation
	netip.MustParsePrefix("2002::/16"),       // NOSONAR: 6to4: embeds an IPv4 address
	netip.MustParsePrefix("2001::/32"),       // NOSONAR: Teredo: embeds an IPv4 address
}

// PublicIP reports whether a is a public unicast address.
func PublicIP(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() ||
		a.IsLinkLocalUnicast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range special {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// CheckURL reports what's wrong with raw as the address of an organization's
// own model server: it must be https, and its host must not name the bot's
// own machine or network. A host name is checked again, once resolved, when
// it's connected to (see Dialer): what it resolves to can change.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case err != nil:
		return fmt.Errorf("%q isn't a valid address: %w", raw, err)
	case u.Scheme != "https":
		return fmt.Errorf("%q must start with https://", raw)
	case u.Host == "" || u.Hostname() == "":
		return fmt.Errorf("%q has no host name", raw)
	case u.User != nil:
		return fmt.Errorf("%q can't have a user name in it; put the key in the API key field", raw)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if a, err := netip.ParseAddr(host); err == nil {
		if !PublicIP(a) {
			return fmt.Errorf("%s isn't a public address", host)
		}
		return nil
	}
	if !strings.Contains(host, ".") || host == "localhost" {
		return fmt.Errorf("%s isn't a public host name", host)
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".intranet", ".corp"} {
		if strings.HasSuffix(host, suffix) {
			return fmt.Errorf("%s isn't a public host name", host)
		}
	}
	return nil
}

// ErrNotPublic is returned when a connection would go to an address that
// isn't public.
var ErrNotPublic = errors.New("refusing to connect to a non-public address")

// control refuses, at connect time, any address that isn't public. It sees
// the address actually dialled, after DNS, so a name that resolves (or is
// rebound) to an internal address is caught too.
func control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNotPublic, address)
	}
	if !PublicIP(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrNotPublic, ap.Addr())
	}
	return nil
}

// Dialer returns a dial function that only connects to public addresses.
func Dialer() func(ctx context.Context, network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: control}
	return d.DialContext
}
