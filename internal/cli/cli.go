// Package cli implements sieve's subcommands.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/qcoderx/sieve/internal/escalate"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/safety"
)

// Run dispatches a subcommand. It returns a process exit code rather than
// calling os.Exit, so the whole CLI stays testable.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "distill":
		return runDistill(rest, stdout, stderr)
	case "replay":
		return runReplay(rest, stdout, stderr)
	case "doctor":
		return runDoctor(rest, stdout, stderr)
	case "bench":
		return runBench(rest, stdout, stderr)
	case "serve":
		return runServe(rest, stdout, stderr)
	case "mcp":
		return runMCP(rest, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "sieve %s\n", render.Version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "sieve: unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `sieve — make a heavy website readable by an agent

Usage:
  sieve <command> [flags]

Commands:
  distill <url>     Produce an artifact for a URL
  replay <file>     Rebuild an artifact from a recorded snapshot, offline
  doctor [url]      Diagnose the environment and, optionally, one page
  bench <dir>       Run a question set against the artifact and the raw page
  serve <dir>       Serve artifacts with content negotiation
  mcp               Run the MCP server on stdio
  version           Print the version

Run "sieve <command> -h" for the flags of a command.

sieve escalates: most pages are answered by a plain HTTP fetch in well under a
second, and the browser is used only where a cheap fetch comes back thin. Every
artifact records which tier answered and why.
`)
}

// commonFlags are shared by the commands that fetch pages.
type commonFlags struct {
	chrome      string
	timeout     time.Duration
	loadTimeout time.Duration
	viewport    string
	tierMin     string
	tierMax     string
	obeyRobots  bool
	allowPriv   bool
	concurrency int
	delay       time.Duration
	verbose     bool
	memoryPath  string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.chrome, "chrome", "", "path to a Chromium binary (default: auto-detect)")
	// Both of these are ceilings, not spends.
	//
	// A page that is ready stops the load wait immediately, and a sweep that
	// stops finding new text ends immediately, so raising either costs a fast
	// page nothing. Measured across example.com, kubernetes.io,
	// news.ycombinator.com and stripe.com, moving from 10s/20s to 45s/45s
	// changed total time by less than the run-to-run noise.
	//
	// What the old values cost was everything else. pear.no recovers 16 of 45
	// ground-truth facts at 10s and 43 at 90s; igloo.inc returns an empty shell
	// at 20s of load budget and its whole site at 40s. Those were being read as
	// extraction failures when they were budget failures, and the default is
	// what nearly every caller uses -- an MCP client never passes a flag.
	fs.DurationVar(&c.timeout, "timeout", 45*time.Second,
		"time budget for reading a page, measured from the moment it has loaded.\n"+
			"A ceiling, not a spend: a sweep that stops finding new text ends early")
	fs.DurationVar(&c.loadTimeout, "load-timeout", 45*time.Second,
		"how long to wait for a slow page to arrive and stop moving before reading it.\n"+
			"Separate from -timeout: a site's own loading time is not work sieve is doing,\n"+
			"and charging it to the extraction only guarantees a thin read")
	fs.StringVar(&c.viewport, "viewport", "1440x900", "viewport size, WxH")
	fs.StringVar(&c.tierMin, "min-tier", "", "force at least this much work: fetch, render, sweep, recover")
	fs.StringVar(&c.tierMax, "max-tier", "", "never work harder than this tier")
	fs.BoolVar(&c.obeyRobots, "robots", true, "obey robots.txt and crawl-delay")
	fs.BoolVar(&c.allowPriv, "allow-private-addresses", false,
		"permit fetching private, loopback and link-local addresses (development only)")
	fs.IntVar(&c.concurrency, "concurrency", 2, "maximum simultaneous requests to one host")
	fs.DurationVar(&c.delay, "delay", 500*time.Millisecond, "minimum interval between requests to one host")
	fs.BoolVar(&c.verbose, "v", false, "log progress")
	fs.StringVar(&c.memoryPath, "memory", defaultMemoryPath(),
		"file holding per-domain escalation memory (empty to disable)")
}

func (c *commonFlags) viewportSize() (int, int, error) {
	parts := strings.SplitN(strings.ToLower(c.viewport), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("viewport %q is not in WxH form", c.viewport)
	}
	var w, h int
	if _, err := fmt.Sscanf(parts[0], "%d", &w); err != nil || w < 320 {
		return 0, 0, fmt.Errorf("viewport width %q is not a sensible number", parts[0])
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &h); err != nil || h < 240 {
		return 0, 0, fmt.Errorf("viewport height %q is not a sensible number", parts[1])
	}
	return w, h, nil
}

func (c *commonFlags) tiers() (min, max escalate.Tier, err error) {
	if c.tierMin != "" {
		t, ok := escalate.ParseTier(c.tierMin)
		if !ok {
			return "", "", fmt.Errorf("unknown tier %q; expected fetch, render, sweep or recover", c.tierMin)
		}
		min = t
	}
	if c.tierMax != "" {
		t, ok := escalate.ParseTier(c.tierMax)
		if !ok {
			return "", "", fmt.Errorf("unknown tier %q; expected fetch, render, sweep or recover", c.tierMax)
		}
		max = t
	}
	if min != "" && max != "" && min.Rank() > max.Rank() {
		return "", "", fmt.Errorf("min-tier %q is above max-tier %q", min, max)
	}
	return min, max, nil
}

// guard builds the SSRF policy.
//
// The default is strict even for the CLI. A URL typed by a user is trustworthy;
// a URL that arrived in a page the user asked to crawl is not, and depth
// crawling makes the second case reachable from the first.
func (c *commonFlags) guard() *safety.Guard {
	cfg := safety.DefaultGuardConfig()
	cfg.AllowPrivate = c.allowPriv
	return safety.NewGuard(cfg)
}

func (c *commonFlags) limiter() *safety.Limiter {
	// Conservative floors that cannot be configured to zero. Publishing an
	// identity is permanent: one badly behaved release makes the project
	// recognisable and blockable forever.
	conc := c.concurrency
	if conc < 1 {
		conc = 1
	}
	if conc > 4 {
		conc = 4
	}
	delay := c.delay
	if delay < 200*time.Millisecond {
		delay = 200 * time.Millisecond
	}
	return safety.NewLimiter(conc, delay)
}

func (c *commonFlags) logf(stderr io.Writer) func(string, ...any) {
	if !c.verbose {
		return nil
	}
	return func(format string, args ...any) {
		fmt.Fprintf(stderr, "  "+format+"\n", args...)
	}
}

// --- escalation memory persistence -----------------------------------------

// defaultMemoryPath puts the escalation memory somewhere durable. Hysteresis
// that only lasts for one process is not hysteresis.
// writeCache writes a small map atomically, or does nothing at all. None of
// these caches is worth failing a run over.
func writeCache(path string, v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Map && rv.Len() == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Remove(path)
	_ = os.Rename(tmp, path)
}

func defaultMemoryPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sieve", "escalation.json")
}

// robotsPath puts the robots cache beside the escalation memory: both are
// per-domain facts learned by running, and both are useless if they last only
// for one process.
func robotsPath(memoryPath string) string {
	if memoryPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(memoryPath), "robots.json")
}

func loadRobots(path string) *safety.RobotsCache {
	c := safety.NewRobotsCache(nil)
	if path == "" {
		return c
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var stored map[string]safety.StoredRobots
	if err := json.Unmarshal(b, &stored); err == nil {
		c.Restore(stored)
	}
	return c
}

func saveRobots(path string, c *safety.RobotsCache) {
	if path == "" || c == nil {
		return
	}
	writeCache(path, c.Snapshot())
}

func loadMemory(path string) *escalate.Memory {
	m := escalate.NewMemory()
	if path == "" {
		return m
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	var stored map[string]string
	if err := json.Unmarshal(b, &stored); err == nil {
		m.Restore(stored)
	}
	return m
}

func saveMemory(path string, m *escalate.Memory) {
	if path == "" {
		return
	}
	writeCache(path, m.Snapshot())
}

// parseArgs parses flags that may appear before or after positional arguments.
//
// Go's flag package stops at the first non-flag argument, which would make
// `sieve distill https://example.com --out ./artifacts` silently ignore --out.
// That is the natural way to type the command and the way it is documented, so
// parsing accommodates it rather than the other way round.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

// --- shared output helpers --------------------------------------------------

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "sieve: %v\n", err)
	return 1
}

func withTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), d)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
