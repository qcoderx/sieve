package safety

import (
	"net"
	"net/url"
	"testing"
)

func TestClassifyIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.170.2", "100.100.100.200", "192.0.0.192",
		"169.254.1.1", "0.0.0.0", "::",
		"100.64.0.1", "198.18.0.1", "203.0.113.9", "240.0.0.1",
		"fd00::1", "fc00::1", "fe80::1", "ff02::1",
		// An IPv4 address written in IPv6 form is the same address.
		"::ffff:127.0.0.1", "::ffff:169.254.169.254",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: %q is not an IP", s)
		}
		if reason := classifyIP(ip); reason == "" {
			t.Errorf("%s should be blocked but was allowed", s)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if reason := classifyIP(ip); reason != "" {
			t.Errorf("%s should be allowed but was blocked as %q", s, reason)
		}
	}
}

func TestGuardLiteralAddresses(t *testing.T) {
	g := NewGuard(DefaultGuardConfig())

	// A literal address must be judged directly rather than looked up, or a
	// resolver that answers for "127.0.0.1" would slip past.
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/",
		"http://192.168.0.1/admin",
	} {
		u, _ := url.Parse(raw)
		if err := g.Check(u); err == nil {
			t.Errorf("%s was allowed", raw)
		}
		g.Reset()
	}
}

func TestGuardSchemes(t *testing.T) {
	g := NewGuard(DefaultGuardConfig())
	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/",
		"data:text/html,<h1>x</h1>",
	} {
		u, _ := url.Parse(raw)
		if err := g.Check(u); err == nil {
			t.Errorf("%s was allowed", raw)
		}
		g.Reset()
	}
}

func TestGuardRedirectBudget(t *testing.T) {
	cfg := DefaultGuardConfig()
	cfg.AllowPrivate = true
	cfg.MaxRedirects = 3
	g := NewGuard(cfg)

	var lastErr error
	for i := 0; i < 10; i++ {
		u, _ := url.Parse("http://example.com/hop" + string(rune('a'+i)))
		lastErr = g.Check(u)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Error("redirect budget was never enforced")
	}
}

func TestHostMatching(t *testing.T) {
	cases := []struct {
		host    string
		pattern string
		want    bool
	}{
		{"example.com", "example.com", true},
		{"EXAMPLE.com", "example.com", true},
		{"example.com.", "example.com", true},
		{"a.example.com", "*.example.com", true},
		{"example.com", "*.example.com", true},
		{"notexample.com", "*.example.com", false},
		{"example.com.evil.net", "example.com", false},
	}
	for _, c := range cases {
		if got := hostMatchesAny(c.host, []string{c.pattern}); got != c.want {
			t.Errorf("hostMatchesAny(%q, %q) = %v, want %v", c.host, c.pattern, got, c.want)
		}
	}
}

func TestRobotsParsing(t *testing.T) {
	txt := `
# comment
User-agent: *
Disallow: /private
Allow: /private/public
Crawl-delay: 2

User-agent: sieve
Disallow: /nope
Allow: /
Crawl-delay: 0.5

Sitemap: https://example.com/sitemap.xml
`
	r := ParseRobots(txt)
	if len(r.Sitemaps) != 1 {
		t.Errorf("sitemaps = %v", r.Sitemaps)
	}
	// The group naming us wins over the wildcard group.
	if r.CrawlDelay.Milliseconds() != 500 {
		t.Errorf("crawl delay = %v, want 500ms", r.CrawlDelay)
	}
	if !r.Allowed("/private") {
		t.Error("/private should be allowed: our own group allows everything but /nope")
	}
	if r.Allowed("/nope") {
		t.Error("/nope should be disallowed")
	}
	if r.Allowed("/nope/deeper") {
		t.Error("/nope/deeper should be disallowed by prefix")
	}
}

func TestRobotsLongestMatchWins(t *testing.T) {
	r := ParseRobots("User-agent: *\nDisallow: /a\nAllow: /a/b\n")
	if r.Allowed("/a") {
		t.Error("/a should be disallowed")
	}
	if !r.Allowed("/a/b") {
		t.Error("/a/b should be allowed: the longer rule wins")
	}
	if !r.Allowed("/a/b/c") {
		t.Error("/a/b/c should be allowed")
	}
}

func TestRobotsEmptyDisallowMeansAllow(t *testing.T) {
	// "Disallow:" with nothing after it permits everything. Confusing it with
	// "Disallow: /" would make sieve refuse to read sites that invited it.
	r := ParseRobots("User-agent: *\nDisallow:\n")
	if !r.Allowed("/anything") {
		t.Error("empty Disallow should permit everything")
	}
	r2 := ParseRobots("User-agent: *\nDisallow: /\n")
	if r2.Allowed("/anything") {
		t.Error("Disallow: / should forbid everything")
	}
}

func TestRobotsWildcards(t *testing.T) {
	r := ParseRobots("User-agent: *\nDisallow: /*.pdf$\nDisallow: /tmp/*/private\n")
	if r.Allowed("/docs/report.pdf") {
		t.Error("*.pdf$ should match")
	}
	if !r.Allowed("/docs/report.pdf.html") {
		t.Error("$ anchor should stop the match")
	}
	if r.Allowed("/tmp/x/private") {
		t.Error("mid-pattern wildcard should match")
	}
	if !r.Allowed("/tmp/x/public") {
		t.Error("non-matching path should be allowed")
	}
}

func TestRobotsMissingIsPermissive(t *testing.T) {
	var r *Robots
	if !r.Allowed("/x") {
		t.Error("nil robots should allow")
	}
	if !(&Robots{Missing: true}).Allowed("/x") {
		t.Error("missing robots.txt should allow")
	}
}
