// Package safety holds the guards that stand between an agent-supplied URL and
// the network: address filtering, robots.txt compliance, and per-domain rate
// limiting.
//
// The threat model is specific. When a CLI user types a URL, it is their own
// machine and their own intent. When an agent calls `distill`, the URL may have
// come from a web page the agent was reading, which means it is attacker
// influenced in the general case. A fetcher that will retrieve any URL it is
// handed is a server-side request forgery primitive, and one running inside a
// cloud environment is a credential disclosure primitive, because instance
// metadata services answer unauthenticated HTTP on a well-known address.
package safety

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrBlocked is the class of error returned when a URL is refused. Callers
// distinguish it from a network failure, because "the site is down" and "we
// declined to connect" are different facts about the world.
var ErrBlocked = errors.New("blocked by safety policy")

// GuardConfig configures address filtering.
type GuardConfig struct {
	// AllowPrivate disables the private-address check. It exists for
	// distilling a site on localhost during development and must never be on
	// for agent-supplied URLs.
	AllowPrivate bool
	// AllowedSchemes defaults to http and https.
	AllowedSchemes []string
	// AllowHosts, when non-empty, is an allowlist: nothing else is fetched.
	AllowHosts []string
	// DenyHosts is applied after AllowHosts.
	DenyHosts []string
	// MaxRedirects bounds a redirect chain.
	MaxRedirects int
	// Resolver is injectable for tests.
	Resolver *net.Resolver
	// LookupTimeout bounds name resolution.
	LookupTimeout time.Duration
}

// DefaultGuardConfig is the policy for agent-supplied URLs.
func DefaultGuardConfig() GuardConfig {
	return GuardConfig{
		AllowedSchemes: []string{"http", "https"},
		MaxRedirects:   8,
		LookupTimeout:  5 * time.Second,
	}
}

// Guard vets URLs before the browser is allowed to open them.
type Guard struct {
	cfg GuardConfig

	mu        sync.Mutex
	redirects int
	seen      map[string]int
}

// NewGuard builds a guard.
func NewGuard(cfg GuardConfig) *Guard {
	if len(cfg.AllowedSchemes) == 0 {
		cfg.AllowedSchemes = []string{"http", "https"}
	}
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = 8
	}
	if cfg.LookupTimeout <= 0 {
		cfg.LookupTimeout = 5 * time.Second
	}
	if cfg.Resolver == nil {
		cfg.Resolver = net.DefaultResolver
	}
	return &Guard{cfg: cfg, seen: map[string]int{}}
}

// Check vets one URL. It is safe for concurrent use and is called for the
// initial navigation and again for every redirect hop, because a URL that
// passed once says nothing about where it points after three hops. Checking
// only the first request is the most common way an SSRF filter is defeated:
// the attacker supplies a benign public host that answers 302 to
// 169.254.169.254.
func (g *Guard) Check(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: nil URL", ErrBlocked)
	}
	if !g.schemeAllowed(u.Scheme) {
		return fmt.Errorf("%w: scheme %q is not allowed", ErrBlocked, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: URL has no host", ErrBlocked)
	}

	g.mu.Lock()
	g.seen[u.String()]++
	n := g.seen[u.String()]
	g.redirects++
	total := g.redirects
	g.mu.Unlock()

	if n > 3 {
		return fmt.Errorf("%w: redirect loop at %s", ErrBlocked, u)
	}
	if total > g.cfg.MaxRedirects+1 {
		return fmt.Errorf("%w: more than %d redirects", ErrBlocked, g.cfg.MaxRedirects)
	}

	if len(g.cfg.AllowHosts) > 0 && !hostMatchesAny(host, g.cfg.AllowHosts) {
		return fmt.Errorf("%w: host %q is not in the allowlist", ErrBlocked, host)
	}
	if hostMatchesAny(host, g.cfg.DenyHosts) {
		return fmt.Errorf("%w: host %q is denied", ErrBlocked, host)
	}

	if g.cfg.AllowPrivate {
		return nil
	}

	// A literal address needs no lookup, and must not get one: resolving
	// "127.0.0.1" as a name is a way to smuggle it past a lookup-based filter.
	if ip := net.ParseIP(host); ip != nil {
		if reason := classifyIP(ip); reason != "" {
			return fmt.Errorf("%w: %s (%s)", ErrBlocked, reason, ip)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.LookupTimeout)
	defer cancel()
	addrs, err := g.cfg.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve %q: %v", ErrBlocked, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %q resolved to no addresses", ErrBlocked, host)
	}
	// Every address is checked, not just the first. A host that resolves to one
	// public address and one loopback address is a rebinding attempt, and which
	// of the two the browser picks is not ours to predict.
	for _, a := range addrs {
		if reason := classifyIP(a.IP); reason != "" {
			return fmt.Errorf("%w: %q resolves to %s (%s)", ErrBlocked, host, a.IP, reason)
		}
	}
	return nil
}

// Reset clears redirect accounting, for reuse across pages of one crawl.
func (g *Guard) Reset() {
	g.mu.Lock()
	g.redirects = 0
	g.seen = map[string]int{}
	g.mu.Unlock()
}

func (g *Guard) schemeAllowed(s string) bool {
	s = strings.ToLower(s)
	for _, a := range g.cfg.AllowedSchemes {
		if s == a {
			return true
		}
	}
	return false
}

// cloudMetadata are the addresses on which cloud platforms serve unauthenticated
// instance metadata, including short-lived credentials. Reaching one of these
// from a URL someone else chose is the difference between a bug and a breach.
var cloudMetadata = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS, GCP, Azure, DigitalOcean, OpenStack
	net.ParseIP("169.254.170.2"),   // AWS ECS task metadata
	net.ParseIP("100.100.100.200"), // Alibaba Cloud
	net.ParseIP("192.0.0.192"),     // Oracle Cloud
	net.ParseIP("fd00:ec2::254"),   // AWS IMDS over IPv6
}

// classifyIP returns a reason string when an address must not be fetched, or
// empty when it is fine.
func classifyIP(ip net.IP) string {
	if ip == nil {
		return "unparseable address"
	}
	for _, m := range cloudMetadata {
		if m != nil && ip.Equal(m) {
			return "cloud instance metadata endpoint"
		}
	}
	if ip.IsLoopback() {
		return "loopback address"
	}
	if ip.IsUnspecified() {
		return "unspecified address"
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "link-local address"
	}
	if ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return "multicast address"
	}
	if ip.IsPrivate() {
		return "private address"
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
			return "carrier-grade NAT range"
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
			return "IETF protocol assignment range"
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 2:
			return "documentation range"
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			return "benchmarking range"
		case v4[0] == 198 && v4[1] == 51 && v4[2] == 100:
			return "documentation range"
		case v4[0] == 203 && v4[1] == 0 && v4[2] == 113:
			return "documentation range"
		case v4[0] >= 240:
			return "reserved range"
		}
		return ""
	}
	// IPv6.
	switch {
	case ip[0] == 0xfc || ip[0] == 0xfd:
		return "unique local address"
	case ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3]&0xf0 == 0x00:
		return "Teredo tunnelling address"
	case ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8:
		return "documentation range"
	}
	// An IPv4 address wrapped in IPv6 notation is still that IPv4 address.
	if v4 := ip.To4(); v4 != nil {
		return classifyIP(v4)
	}
	return ""
}

// hostMatchesAny supports exact hosts and leading-wildcard patterns.
func hostMatchesAny(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "*.") {
			suffix := p[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) || host == p[2:] {
				return true
			}
			continue
		}
		if host == p {
			return true
		}
	}
	return false
}
