package safety

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UserAgent is the token sieve identifies itself with, in robots.txt and in the
// User-Agent header. It carries a contact URL because a site operator who wants
// this traffic to stop should not have to guess who to ask.
const UserAgent = "sieve"

// Robots is a parsed robots.txt for one origin.
type Robots struct {
	groups []robotGroup
	// CrawlDelay is the delay the most specific matching group asked for.
	CrawlDelay time.Duration
	// Sitemaps are advertised sitemap URLs, useful for a bounded crawl.
	Sitemaps []string
	// Missing is true when the file did not exist, which means everything is
	// allowed. It is recorded so the artifact can say which it was.
	Missing bool
}

type robotGroup struct {
	agents []string
	rules  []robotRule
	delay  time.Duration
}

type robotRule struct {
	path  string
	allow bool
}

// FetchRobots retrieves and parses robots.txt for a URL's origin.
//
// A robots.txt that cannot be fetched is treated as permissive, which is what
// the standard and every major crawler do: a 404 means no restrictions, and a
// network error means we could not ask. A 401 or 403 on robots.txt itself is
// treated as a refusal of the whole site, because a site that will not even
// show its rules has not invited us in.
func FetchRobots(ctx context.Context, client *http.Client, target *url.URL) (*Robots, error) {
	ru := &url.URL{Scheme: target.Scheme, Host: target.Host, Path: "/robots.txt"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent+"/0.1 (+https://github.com/qcoderx/sieve)")
	req.Header.Set("Accept", "text/plain,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return &Robots{Missing: true}, nil
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: robots.txt returned %d, treating the whole site as disallowed",
			ErrBlocked, resp.StatusCode)
	case resp.StatusCode >= 400:
		return &Robots{Missing: true}, nil
	}

	// robots.txt files are small by convention. Reading an unbounded body from
	// an untrusted host is how a fetcher gets turned into a memory exhaustion
	// target.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return &Robots{Missing: true}, nil
	}
	return ParseRobots(string(body)), nil
}

// ParseRobots parses robots.txt content.
func ParseRobots(s string) *Robots {
	r := &Robots{}
	var cur *robotGroup
	// Consecutive User-agent lines share one group of rules; a rule line ends
	// the run of agents.
	startingGroup := false

	for _, raw := range strings.Split(s, "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		field := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])

		switch field {
		case "user-agent":
			if cur == nil || !startingGroup {
				r.groups = append(r.groups, robotGroup{})
				cur = &r.groups[len(r.groups)-1]
				startingGroup = true
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
		case "disallow", "allow":
			if cur == nil {
				continue
			}
			startingGroup = false
			// "Disallow:" with an empty value means allow everything, which is
			// the opposite of "Disallow: /" and must not be confused with it.
			if field == "disallow" && value == "" {
				continue
			}
			cur.rules = append(cur.rules, robotRule{path: value, allow: field == "allow"})
		case "crawl-delay":
			if cur == nil {
				continue
			}
			startingGroup = false
			if f, err := strconv.ParseFloat(value, 64); err == nil && f > 0 {
				cur.delay = time.Duration(f * float64(time.Second))
			}
		case "sitemap":
			r.Sitemaps = append(r.Sitemaps, value)
		}
	}

	if g := r.matchGroup(); g != nil {
		r.CrawlDelay = g.delay
	}
	return r
}

// matchGroup picks the group that applies to us: an exact agent match if there
// is one, otherwise the wildcard group.
func (r *Robots) matchGroup() *robotGroup {
	var wildcard *robotGroup
	for i := range r.groups {
		for _, a := range r.groups[i].agents {
			if a == UserAgent {
				return &r.groups[i]
			}
			if a == "*" {
				wildcard = &r.groups[i]
			}
		}
	}
	return wildcard
}

// Allowed reports whether a path may be fetched.
//
// The matching rule is longest-match-wins with allow beating disallow on a tie,
// which is what Google's specification defines and what site operators write
// their files expecting.
func (r *Robots) Allowed(path string) bool {
	if r == nil || r.Missing {
		return true
	}
	g := r.matchGroup()
	if g == nil || len(g.rules) == 0 {
		return true
	}
	if path == "" {
		path = "/"
	}

	type match struct {
		length int
		allow  bool
	}
	var matches []match
	for _, rule := range g.rules {
		if n, ok := matchPattern(rule.path, path); ok {
			matches = append(matches, match{n, rule.allow})
		}
	}
	if len(matches) == 0 {
		return true
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].length != matches[j].length {
			return matches[i].length > matches[j].length
		}
		return matches[i].allow && !matches[j].allow
	})
	return matches[0].allow
}

// matchPattern implements robots.txt path matching, including the `*` wildcard
// and the `$` end anchor. It returns the length of the matched pattern, which
// is what the longest-match rule compares.
func matchPattern(pattern, path string) (int, bool) {
	if pattern == "" {
		return 0, false
	}
	anchored := strings.HasSuffix(pattern, "$")
	p := pattern
	if anchored {
		p = p[:len(p)-1]
	}
	if !strings.Contains(p, "*") {
		if !strings.HasPrefix(path, p) {
			return 0, false
		}
		if anchored && len(path) != len(p) {
			return 0, false
		}
		return len(pattern), true
	}

	// Wildcard match: consume literal segments in order.
	parts := strings.Split(p, "*")
	pos := 0
	for i, seg := range parts {
		if seg == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(path, seg) {
				return 0, false
			}
			pos = len(seg)
			continue
		}
		j := strings.Index(path[pos:], seg)
		if j < 0 {
			return 0, false
		}
		pos += j + len(seg)
	}
	if anchored && pos != len(path) {
		// The final literal has to land exactly at the end, unless the pattern
		// ended with a wildcard.
		if parts[len(parts)-1] != "" {
			return 0, false
		}
	}
	return len(pattern), true
}

// RobotsCache holds one parsed robots.txt per origin for the life of a job.
type RobotsCache struct {
	client *http.Client
	mu     sync.Mutex
	byHost map[string]*robotsEntry
}

type robotsEntry struct {
	once sync.Once
	r    *Robots
	err  error
}

// NewRobotsCache builds a cache.
func NewRobotsCache(client *http.Client) *RobotsCache {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &RobotsCache{client: client, byHost: map[string]*robotsEntry{}}
}

// Get fetches robots.txt for a URL's origin, at most once per origin.
func (c *RobotsCache) Get(ctx context.Context, u *url.URL) (*Robots, error) {
	key := u.Scheme + "://" + u.Host
	c.mu.Lock()
	e, ok := c.byHost[key]
	if !ok {
		e = &robotsEntry{}
		c.byHost[key] = e
	}
	c.mu.Unlock()

	e.once.Do(func() {
		e.r, e.err = FetchRobots(ctx, c.client, u)
	})
	return e.r, e.err
}

// Allowed is the convenience form: fetch the rules and apply them.
func (c *RobotsCache) Allowed(ctx context.Context, u *url.URL) error {
	r, err := c.Get(ctx, u)
	if err != nil {
		return err
	}
	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if !r.Allowed(path) {
		return fmt.Errorf("%w: robots.txt disallows %s for user-agent %q", ErrBlocked, path, UserAgent)
	}
	return nil
}
