// Package serve hosts distilled artifacts with content negotiation.
//
// The negotiation rule is simple and it is the point: an agent gets the
// distilled content, a human gets sent to the real site. A person should read
// the page the designer built; an agent should read the version that costs a
// tenth of the tokens.
package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
)

// Options configures the server.
type Options struct {
	// Root is the directory holding artifact directories.
	Root string
	// RedirectHumans sends browsers to the original URL.
	RedirectHumans bool
}

// Handler serves artifacts.
type Handler struct {
	opts Options

	mu    sync.RWMutex
	byDir map[string]*graph.Graph
	names []string
}

// New scans a directory of artifacts.
func New(opts Options) (*Handler, error) {
	h := &Handler{opts: opts, byDir: map[string]*graph.Graph{}}
	entries, err := os.ReadDir(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", opts.Root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(opts.Root, e.Name())
		g, err := emit.LoadGraph(dir)
		if err != nil {
			continue
		}
		h.byDir[e.Name()] = g
		h.names = append(h.names, e.Name())
	}
	sort.Strings(h.names)
	if len(h.names) == 0 {
		return nil, fmt.Errorf("no artifacts found in %s", opts.Root)
	}
	return h, nil
}

// Artifacts lists the served artifact names.
func (h *Handler) Artifacts() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]string(nil), h.names...)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + r.URL.Path)
	if clean == "/" {
		h.index(w, r)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(clean, "/"), "/", 2)
	name := parts[0]

	h.mu.RLock()
	g := h.byDir[name]
	h.mu.RUnlock()
	if g == nil {
		http.NotFound(w, r)
		return
	}

	// Content-addressed: an unchanged artifact is a 304 rather than a resend.
	etag := `"` + strings.TrimPrefix(g.ContentHash, "sha256:")[:16] + `"`
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("X-Sieve-Content-Hash", g.ContentHash)
	w.Header().Set("X-Sieve-Tier", g.Provenance.Tier)
	w.Header().Set("Vary", "Accept, User-Agent")

	// A private artifact came from an authenticated session and must never be
	// served from a shared instance.
	if g.Provenance.Private {
		http.Error(w, "this artifact was produced from a private session and is not served publicly",
			http.StatusForbidden)
		return
	}

	switch {
	case wantsJSON(r):
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(g)
	case wantsMarkdown(r):
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, emit.Markdown(g, emit.DefaultMarkdownOptions()))
	case isAgent(r) || !h.opts.RedirectHumans:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, emit.HTML(g))
	default:
		// A human gets the site the designer built.
		target := g.FinalURL
		if target == "" {
			target = g.URL
		}
		http.Redirect(w, r, target, http.StatusFound)
	}
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		out := make([]map[string]any, 0, len(h.names))
		for _, n := range h.names {
			g := h.byDir[n]
			out = append(out, map[string]any{
				"path": "/" + n, "url": g.URL, "title": g.Title,
				"content_hash": g.ContentHash, "tier": g.Provenance.Tier,
				"est_tokens": g.Stats.ArtifactTokens,
			})
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>sieve artifacts</title>"+
		"<style>body{font:16px/1.6 ui-sans-serif,system-ui,sans-serif;max-width:44rem;margin:2rem auto;padding:0 1rem}"+
		"li{margin:.4rem 0}code{opacity:.6;font-size:.85em}</style>"+
		"<h1>sieve artifacts</h1><ul>")
	for _, n := range h.names {
		g := h.byDir[n]
		fmt.Fprintf(w, "<li><a href=\"/%s\">%s</a><br><code>%s — %s tier, ~%d tokens</code></li>",
			n, htmlEscape(g.Title), htmlEscape(g.URL), g.Provenance.Tier, g.Stats.ArtifactTokens)
	}
	fmt.Fprint(w, "</ul>")
}

// agentUserAgents are the tokens that identify an automated reader. The list is
// short on purpose: guessing wrong sends a human the stripped version, which is
// a worse failure than sending an agent the real page.
var agentUserAgents = []string{
	"claude", "anthropic", "gptbot", "chatgpt", "openai", "perplexity",
	"bingbot", "google-extended", "cohere", "bytespider", "ccbot",
	"applebot-extended", "sieve", "curl", "wget", "httpie", "python-requests",
	"go-http-client", "node-fetch", "axios", "libwww-perl",
}

func isAgent(r *http.Request) bool {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	if ua == "" {
		return true
	}
	for _, t := range agentUserAgents {
		if strings.Contains(ua, t) {
			return true
		}
	}
	return false
}

func wantsMarkdown(r *http.Request) bool {
	return acceptsType(r, "text/markdown") || acceptsType(r, "text/x-markdown")
}

func wantsJSON(r *http.Request) bool {
	return acceptsType(r, "application/json")
}

// acceptsType reports whether an Accept header explicitly names a type. A
// wildcard does not count: "*/*" is what every browser sends, and treating it
// as a request for markdown would send the distilled version to humans.
func acceptsType(r *http.Request, want string) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(mediaType, want) {
			return true
		}
	}
	return false
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
