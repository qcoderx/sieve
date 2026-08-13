// Package fetch is tier 0: a plain HTTP GET, and the cheap checks that decide
// whether anything has changed since last time.
//
// # Why change detection needs its own ladder
//
// The artifact cache is keyed on content hash, which sounds like it makes
// re-distilling an unchanged site free. It does not, quite: computing that hash
// requires rendering the site, which is the expensive step. "Re-distilling an
// unchanged site is free" is really "storing an unchanged site is free" unless
// something cheaper can prove the page has not moved.
//
// So there is a ladder, cheapest first, and most sites reveal the answer on the
// first or second rung:
//
//  1. A conditional request. A 304 ends the question at almost no cost.
//  2. Sitemap lastmod, where a sitemap exists.
//  3. The raw HTML byte hash. Noisy -- it changes on every rebuild -- but a
//     match is conclusive proof of no change.
//  4. The static-text hash, taken after boilerplate removal. This survives
//     asset-hash churn and build-id rotation, which is what makes rung 3 noisy.
//  5. Render.
//
// Only genuine ambiguity reaches rung 5.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qcoderx/sieve/internal/safety"
	"github.com/qcoderx/sieve/internal/static"
)

// UserAgent identifies sieve honestly, with a contact URL.
//
// Publishing an identity is permanent: once there is a documented user agent
// and a contact address, one badly behaved release makes the project
// recognisable and blockable forever. That is the argument for conservative
// rate limits that cannot be configured away, not for hiding.
const UserAgent = "Mozilla/5.0 (compatible; sieve/0.1; +https://github.com/qcoderx/sieve)"

// Options configures the fetcher.
type Options struct {
	// Timeout bounds a single request.
	Timeout time.Duration
	// MaxBytes bounds a response body. Remote input is untrusted, and an
	// unbounded read from an arbitrary host is a memory-exhaustion primitive.
	MaxBytes int64
	// MaxRedirects bounds a redirect chain.
	MaxRedirects int
	// Guard vets every URL, including every redirect hop.
	Guard *safety.Guard
	// AcceptLanguage is pinned rather than inherited.
	AcceptLanguage string
}

// DefaultOptions returns sane, conservative settings.
func DefaultOptions() Options {
	return Options{
		Timeout:        20 * time.Second,
		MaxBytes:       16 << 20,
		MaxRedirects:   8,
		AcceptLanguage: "en-US,en;q=0.9",
	}
}

// Response is a fetched document plus the freshness signals it carried.
type Response struct {
	URL         string
	FinalURL    string
	Status      int
	Body        []byte
	ContentType string
	// ETag and LastModified are the rung-1 signals, stored so the next run can
	// make a conditional request.
	ETag         string
	LastModified string
	// NotModified reports that the server answered 304.
	NotModified bool
	// Blocked reports that the response looks like a refusal rather than a page.
	Blocked       bool
	BlockedReason string
	Elapsed       time.Duration
	// Redirects is the chain that was followed, recorded because a redirect to
	// a login page is a common and otherwise invisible failure.
	Redirects []string
}

// Freshness is what a previous run stored so the next one can skip work.
type Freshness struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	// RawHash is the byte hash of the served HTML. It changes on every rebuild
	// even when nothing a reader would notice changed, so a mismatch proves
	// nothing -- but a match is conclusive.
	RawHash string `json:"raw_hash,omitempty"`
	// TextHash is the hash of the extracted static text. It survives asset
	// churn and build-id rotation, which is exactly what makes RawHash noisy.
	TextHash string `json:"text_hash,omitempty"`
	// ContentHash is the artifact's own semantic hash from the last run.
	ContentHash string    `json:"content_hash,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
	// TTL is how long this domain has earned before being re-checked.
	TTL time.Duration `json:"ttl,omitempty"`
}

// ChangeVerdict is the outcome of the ladder.
type ChangeVerdict struct {
	Changed bool
	// Rung names which check answered, so the manifest can say how the
	// freshness conclusion was reached rather than merely asserting it.
	Rung string
	// Confident reports whether the check is conclusive. A raw-hash match is
	// conclusive; a raw-hash mismatch is not.
	Confident bool
	Response  *Response
}

// Client performs tier-0 fetches.
type Client struct {
	http *http.Client
	opts Options
}

// New builds a client whose transport refuses to connect to addresses the
// guard rejects.
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 16 << 20
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = 8
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	c := &Client{opts: opts}
	c.http = &http.Client{
		Transport: tr,
		Timeout:   opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= opts.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", opts.MaxRedirects)
			}
			// Every hop is vetted, not just the first. A URL that passed once
			// says nothing about where it points after three redirects, and
			// checking only the first request is the most common way an SSRF
			// filter is defeated.
			if opts.Guard != nil {
				if err := opts.Guard.Check(req.URL); err != nil {
					return err
				}
			}
			return nil
		},
	}
	return c
}

// Get fetches a URL, optionally conditionally.
func (c *Client) Get(ctx context.Context, rawURL string, prior *Freshness) (*Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if c.opts.Guard != nil {
		if err := c.opts.Guard.Check(u); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", c.opts.AcceptLanguage)
	// Rung 1 of the change ladder. A 304 ends the whole question here.
	if prior != nil {
		if prior.ETag != "" {
			req.Header.Set("If-None-Match", prior.ETag)
		}
		if prior.LastModified != "" {
			req.Header.Set("If-Modified-Since", prior.LastModified)
		}
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, safety.ErrBlocked) {
			return nil, err
		}
		var uerr *url.Error
		if errors.As(err, &uerr) && errors.Is(uerr.Err, safety.ErrBlocked) {
			return nil, uerr.Err
		}
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	out := &Response{
		URL:          rawURL,
		FinalURL:     resp.Request.URL.String(),
		Status:       resp.StatusCode,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		NotModified:  resp.StatusCode == http.StatusNotModified,
		Elapsed:      time.Since(start),
	}
	out.Blocked, out.BlockedReason = detectBlock(resp)

	if out.NotModified {
		return out, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	out.Body = body
	return out, nil
}

// CheckChanged walks the change-detection ladder.
func (c *Client) CheckChanged(ctx context.Context, rawURL string, prior *Freshness) (*ChangeVerdict, error) {
	if prior == nil {
		return &ChangeVerdict{Changed: true, Rung: "no prior artifact", Confident: true}, nil
	}
	// A domain that has earned a long TTL is not re-checked at all until it
	// expires. A marketing site that changed twice in a year does not need a
	// conditional request every hour.
	if prior.TTL > 0 && time.Since(prior.CheckedAt) < prior.TTL {
		return &ChangeVerdict{
			Changed: false, Confident: false,
			Rung: fmt.Sprintf("within the learned %s TTL for this domain", prior.TTL),
		}, nil
	}

	resp, err := c.Get(ctx, rawURL, prior)
	if err != nil {
		return nil, err
	}

	// Rung 1: the server answered the question directly.
	if resp.NotModified {
		return &ChangeVerdict{Changed: false, Rung: "HTTP 304 Not Modified", Confident: true, Response: resp}, nil
	}
	if prior.ETag != "" && resp.ETag != "" && prior.ETag == resp.ETag {
		return &ChangeVerdict{Changed: false, Rung: "unchanged ETag", Confident: true, Response: resp}, nil
	}

	// Rung 3: the raw bytes. A match is conclusive; a mismatch says nothing,
	// because a rebuild rotates asset hashes without changing a word.
	rawHash := HashBytes(resp.Body)
	if prior.RawHash != "" && prior.RawHash == rawHash {
		return &ChangeVerdict{Changed: false, Rung: "identical served HTML", Confident: true, Response: resp}, nil
	}

	// Rung 4: the extracted text. This is the rung that catches the common
	// case -- a redeployed site whose content did not move.
	if prior.TextHash != "" {
		res, err := static.Extract(resp.FinalURL, strings.NewReader(string(resp.Body)), len(resp.Body))
		if err == nil {
			if HashString(staticText(res)) == prior.TextHash {
				return &ChangeVerdict{
					Changed: false, Confident: true, Response: resp,
					Rung: "served HTML differs but its extracted text is identical",
				}, nil
			}
		}
	}

	return &ChangeVerdict{Changed: true, Rung: "extracted text differs", Confident: true, Response: resp}, nil
}

// staticText renders a static extraction to the string that gets hashed.
func staticText(r *static.Result) string {
	var sb strings.Builder
	for _, n := range r.Merged.Nodes {
		sb.WriteString(n.Text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// StaticTextHash is the rung-4 signal for a fetched document.
func StaticTextHash(r *static.Result) string { return HashString(staticText(r)) }

// HashBytes and HashString produce the stored freshness signals.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:32]
}

func HashString(s string) string { return HashBytes([]byte(s)) }

// LearnTTL adjusts how long a domain is trusted between checks.
//
// This is a small table, not a model: a site that keeps coming back unchanged
// earns a longer interval, and one that changes earns a short one. The ceiling
// exists because an artifact that is a week stale without saying so is worse
// than one that costs a conditional request.
func LearnTTL(prev time.Duration, changed bool) time.Duration {
	const (
		floor   = 5 * time.Minute
		ceiling = 24 * time.Hour
	)
	if changed {
		return floor
	}
	next := prev * 2
	if next < floor {
		next = floor
	}
	if next > ceiling {
		next = ceiling
	}
	return next
}

// detectBlock reports whether a response is a refusal rather than a page.
//
// Telling these apart matters: a refusal degrades to a labelled partial
// artifact, while a genuine failure is an error. Reporting a challenge page as
// content would be the worst of both.
func detectBlock(resp *http.Response) (bool, string) {
	switch resp.StatusCode {
	case http.StatusForbidden:
		return true, "HTTP 403"
	case http.StatusTooManyRequests:
		return true, "HTTP 429 rate limited"
	case http.StatusServiceUnavailable:
		return true, "HTTP 503"
	}
	if v := resp.Header.Get("cf-mitigated"); strings.Contains(strings.ToLower(v), "challenge") {
		return true, "Cloudflare challenge"
	}
	for _, h := range []struct{ key, name string }{
		{"x-datadome", "DataDome"},
		{"x-iinfo", "Imperva Incapsula"},
		{"x-sucuri-id", "Sucuri"},
		{"x-kasada-classification", "Kasada"},
	} {
		if resp.Header.Get(h.key) != "" && resp.StatusCode >= 400 {
			return true, h.name
		}
	}
	return false, ""
}

// IsHTML reports whether a response is worth parsing as a document.
func (r *Response) IsHTML() bool {
	ct := strings.ToLower(r.ContentType)
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		return true
	}
	// A server that sends no content type at all is common enough that sniffing
	// the first bytes is worth it.
	if ct == "" && len(r.Body) > 0 {
		head := strings.ToLower(strings.TrimSpace(string(r.Body[:min(512, len(r.Body))])))
		return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
