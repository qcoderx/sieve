package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/escalate"
	"github.com/qcoderx/sieve/internal/fetch"
	"github.com/qcoderx/sieve/internal/llm"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/static"
)

// runDoctor produces an attachable diagnostic bundle.
//
// This exists because of a specific, recurring failure mode: a user reports
// "it didn't work on my site", and the maintainer has no way to tell whether
// the problem was a missing browser, a starved compositor, a scroll hijacker,
// an escalation decision that stopped at tier 0, or a genuine extraction bug.
// Every one of those has a different fix and they are indistinguishable from
// the outside.
//
// Doctor answers all of them in one command, and the output is designed to be
// pasted into an issue.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		common   commonFlags
		asJSON   bool
	)
	common.register(fs)
	fs.BoolVar(&asJSON, "json", false, "emit the diagnostic bundle as JSON")

	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: sieve doctor [url] [flags]

Checks the environment, and optionally probes one page.

With no URL it reports what sieve found on this machine. With a URL it also
fetches the page, scores the escalation decision, and -- if a browser is
available -- verifies that frames are actually being produced, which is the
failure that silently turns a rich page into an empty artifact.

Paste the output into a bug report.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, perr := parseArgs(fs, args)
	if perr != nil {
		return 2
	}

	d := diagnosis{
		SieveVersion: render.Version,
		Go:           runtime.Version(),
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		CaptureHash:  capture.ScriptHash(),
		Checks:       []check{},
	}

	// --- environment --------------------------------------------------------
	chromePath := render.ChromiumPath(common.chrome)
	if chromePath == "" {
		d.add("browser", false, "no Chromium, Chrome or Edge binary found",
			"Install a Chromium-based browser, or pass --chrome /path/to/chrome. "+
				"Tier 0 still works without one; anything above it does not.")
	} else {
		d.add("browser", true, chromePath, "")
	}
	d.ChromePath = chromePath

	if llm.HasCredentials("") {
		d.add("anthropic-credentials", true, "found", "")
	} else {
		d.add("anthropic-credentials", false, "not configured",
			"Only the vision and benchmark paths need these. Distillation does not.")
	}

	if specs, err := render.LibrarySpecs(); err != nil {
		d.add("fingerprints", false, err.Error(), "The embedded detector file failed to parse.")
	} else {
		d.add("fingerprints", true, fmt.Sprintf("%d library detectors loaded", len(specs)), "")
	}

	ctx, cancel := withTimeout(common.timeout)
	defer cancel()

	// --- browser probe ------------------------------------------------------
	if chromePath != "" {
		d.probeBrowser(ctx, common, chromePath)
	}

	// --- page probe ---------------------------------------------------------
	if len(positional) == 1 {
		d.probePage(ctx, common, positional[0])
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(d); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	d.print(stdout)
	if d.failed() {
		return 1
	}
	return 0
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Advice string `json:"advice,omitempty"`
}

type diagnosis struct {
	SieveVersion string  `json:"sieve_version"`
	Go           string  `json:"go"`
	OS           string  `json:"os"`
	Arch         string  `json:"arch"`
	CaptureHash  string  `json:"capture_script_sha256"`
	ChromePath   string  `json:"chrome_path,omitempty"`
	Chromium     string  `json:"chromium_version,omitempty"`
	Checks       []check `json:"checks"`

	Page *pageDiagnosis `json:"page,omitempty"`
}

type pageDiagnosis struct {
	URL         string             `json:"url"`
	FinalURL    string             `json:"final_url,omitempty"`
	Status      int                `json:"status"`
	Bytes       int                `json:"bytes"`
	Blocked     bool               `json:"blocked,omitempty"`
	BlockedWhy  string             `json:"blocked_reason,omitempty"`
	Signals     static.Signals     `json:"signals"`
	Decision    escalate.Decision  `json:"decision"`
	Factors     []escalate.Factor  `json:"factors"`
	FetchMillis int64              `json:"fetch_ms"`
}

func (d *diagnosis) add(name string, ok bool, detail, advice string) {
	d.Checks = append(d.Checks, check{Name: name, OK: ok, Detail: detail, Advice: advice})
}

func (d *diagnosis) failed() bool {
	for _, c := range d.Checks {
		if !c.OK && c.Name != "anthropic-credentials" {
			return true
		}
	}
	return false
}

// probeBrowser verifies the thing that no documentation warns about.
//
// A headless tab that is not being composited never runs the rendering steps,
// which starves requestAnimationFrame and therefore IntersectionObserver -- the
// mechanism nearly every scroll-reveal animation uses to decide when to show
// content. Under that condition a sweep completes quickly, reports no errors,
// and produces an artifact containing the hero and nothing else.
//
// It has bitten this project twice: once because secondary tabs are not
// activated by default, and once because --in-process-gpu with SwiftShader
// stops frame production for every tab after the first. Both are invisible
// without a direct probe, so doctor runs one.
func (d *diagnosis) probeBrowser(ctx context.Context, common commonFlags, chromePath string) {
	opts := render.DefaultOptions()
	opts.ChromePath = chromePath
	opts.Logf = nil

	bctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	b, err := render.Launch(bctx, opts)
	if err != nil {
		d.add("browser-launch", false, err.Error(),
			"sieve could not start the browser it found.")
		return
	}
	defer b.Close()
	d.add("browser-launch", true, "started", "")

	frames, verr := b.ProbeFrameProduction(bctx)
	if verr != nil {
		d.add("frame-production", false, verr.Error(),
			"The probe itself failed, which usually means the tab never became reachable.")
		return
	}
	if frames.Ticks >= 3 {
		d.add("frame-production", true,
			fmt.Sprintf("%d animation frames in %v on a secondary tab", frames.Ticks, frames.Elapsed.Round(time.Millisecond)), "")
	} else {
		d.add("frame-production", false,
			fmt.Sprintf("only %d animation frames in %v", frames.Ticks, frames.Elapsed.Round(time.Millisecond)),
			"requestAnimationFrame is starved on this machine. IntersectionObserver will not fire, "+
				"so scroll-reveal content will never become visible and artifacts will look almost empty. "+
				"This is usually a GPU flag interaction; report it with this output attached.")
	}
	if frames.IO {
		d.add("intersection-observer", true, "callbacks delivered", "")
	} else {
		d.add("intersection-observer", false, "no callback within the probe window",
			"Scroll-reveal content will not be captured on this machine.")
	}
	d.Chromium = b.ChromiumVersion()
}

func (d *diagnosis) probePage(ctx context.Context, common commonFlags, target string) {
	fo := fetch.DefaultOptions()
	fo.Guard = common.guard()
	c := fetch.New(fo)

	start := time.Now()
	resp, err := c.Get(ctx, target, nil)
	if err != nil {
		d.add("page-fetch", false, err.Error(), "")
		return
	}
	d.add("page-fetch", true, fmt.Sprintf("HTTP %d, %d bytes in %v",
		resp.Status, len(resp.Body), resp.Elapsed.Round(time.Millisecond)), "")

	if resp.Blocked {
		d.add("page-access", false, resp.BlockedReason,
			"The site refused this client. sieve respects that rather than working around it; "+
				"the artifact would be built from the served HTML alone and labelled as blocked.")
	}

	res, serr := static.Extract(resp.FinalURL, strings.NewReader(string(resp.Body)), len(resp.Body))
	if serr != nil {
		d.add("static-extraction", false, serr.Error(), "")
		return
	}
	dec := escalate.Score(res.Signals, 0, "", escalate.DefaultThresholds())

	d.Page = &pageDiagnosis{
		URL: target, FinalURL: resp.FinalURL, Status: resp.Status,
		Bytes: len(resp.Body), Blocked: resp.Blocked, BlockedWhy: resp.BlockedReason,
		Signals: res.Signals, Decision: dec, Factors: dec.Factors,
		FetchMillis: time.Since(start).Milliseconds(),
	}
	d.add("escalation", true,
		fmt.Sprintf("tier %q at score %.3f", dec.Tier, dec.Score), dec.Reason)
}

func (d *diagnosis) print(w io.Writer) {
	fmt.Fprintf(w, "sieve %s  (go %s, %s/%s)\n", d.SieveVersion, d.Go, d.OS, d.Arch)
	fmt.Fprintf(w, "capture script  %s\n", d.CaptureHash)
	if d.Chromium != "" {
		fmt.Fprintf(w, "chromium        %s\n", d.Chromium)
	}
	fmt.Fprintln(w)

	for _, c := range d.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %-22s %s\n", mark, c.Name, c.Detail)
		if c.Advice != "" {
			for _, line := range wrap(c.Advice, 68) {
				fmt.Fprintf(w, "         %s\n", line)
			}
		}
	}

	if d.Page != nil {
		p := d.Page
		fmt.Fprintf(w, "\npage  %s\n", p.URL)
		if p.FinalURL != p.URL {
			fmt.Fprintf(w, "  redirected to %s\n", p.FinalURL)
		}
		s := p.Signals
		fmt.Fprintf(w, "  served      %d bytes, %d characters of text (%.2f%%)\n",
			s.HTMLBytes, s.TextChars, s.TextRatio*100)
		fmt.Fprintf(w, "  structure   %d headings, %d paragraphs, %d landmarks, %d links\n",
			s.Headings, s.Paragraphs, s.Landmarks, s.Links)
		fmt.Fprintf(w, "  scripts     %d bytes; hydration blob %v; noscript warning %v\n",
			s.ScriptBytes, s.HydrationBlob, s.NoScriptWarning)
		fmt.Fprintf(w, "  canvas      %d element(s)\n", s.CanvasElements)
		fmt.Fprintf(w, "\n  decision    tier %q, score %.3f\n", p.Decision.Tier, p.Decision.Score)
		for _, f := range p.Factors {
			if f.Note == "" {
				continue
			}
			fmt.Fprintf(w, "    %-16s %-10v %s\n", f.Name, f.Value, f.Note)
		}
	}

	fmt.Fprintln(w)
	if d.failed() {
		fmt.Fprintln(w, "One or more checks failed. Attach this output to a bug report.")
	} else {
		fmt.Fprintln(w, "All checks passed.")
	}
}

func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out = append(out, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(out, line)
}
