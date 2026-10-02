package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

var (
	errBadTarget  = errors.New("invalid target")
	errNotPublic  = errors.New("target is not a public address")
	errNoAddress  = errors.New("hostname has no usable address")
	hostnameRegex = regexp.MustCompile(`^(?i)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}\.?$`)
)

// Prefixes that are never valid targets, on top of what netip already
// classifies as private, loopback, link-local or multicast.
var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",       // "this" network
	"100.64.0.0/10",   // CGNAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // deprecated 6to4 relay anycast
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved + broadcast
	"64:ff9b::/96",    // NAT64
	"64:ff9b:1::/48",  // local-use NAT64
	"100::/64",        // discard-only
	"2001::/23",       // IETF protocol assignments
	"2001:db8::/32",   // documentation
	"2002::/16",       // 6to4
	"3fff::/20",       // documentation
	"5f00::/16",       // SRv6 SIDs
)

var globalUnicast6 = netip.MustParsePrefix("2000::/3")

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// isPublic reports whether addr is a globally routable unicast address.
func isPublic(addr netip.Addr, extra []netip.Prefix) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	// For IPv6, allow only the space IANA delegates for global unicast. This
	// also rules out deprecated site-local fec0::/10 and unassigned space.
	if addr.Is6() && !globalUnicast6.Contains(addr) {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	for _, p := range extra {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// resolveTarget turns user input into a single public IP of the requested
// family (4 or 6). The returned address is what gets passed to the tools,
// never the raw input, so hostnames cannot smuggle arguments and DNS cannot
// be re-resolved to something else between the check and the use.
func resolveTarget(ctx context.Context, input string, family int, extra []netip.Prefix) (netip.Addr, error) {
	input = strings.TrimSpace(input)
	if input == "" || len(input) > 253 {
		return netip.Addr{}, errBadTarget
	}

	if addr, err := netip.ParseAddr(input); err == nil {
		addr = addr.Unmap()
		if (family == 4) != addr.Is4() {
			return netip.Addr{}, errNoAddress
		}
		if !isPublic(addr, extra) {
			return netip.Addr{}, errNotPublic
		}
		return addr, nil
	}

	if !hostnameRegex.MatchString(input) {
		return netip.Addr{}, errBadTarget
	}

	network := "ip4"
	if family == 6 {
		network = "ip6"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, network, input)
	if err != nil {
		return netip.Addr{}, errNoAddress
	}
	for _, a := range addrs {
		a = a.Unmap()
		if isPublic(a, extra) {
			return a, nil
		}
	}
	if len(addrs) > 0 {
		return netip.Addr{}, errNotPublic
	}
	return netip.Addr{}, errNoAddress
}
