// Package escalate decides how hard to work on a page.
//
// # The decision, and the cost of getting it wrong
//
// Escalation is what makes sieve correct at the appropriate cost across the
// whole web rather than a heavy tool that is overkill for a blog. It is also
// the change most capable of damaging the project, because a tool that judges
// the same page differently on different days has forfeited the claim that the
// same input yields the same output -- and pages near a threshold are exactly
// where that happens.
//
// Three things keep the decision stable, and all three are load-bearing:
//
//  1. The thresholds are pinned constants, not values computed per run. A
//     threshold derived from the page being judged can move under it.
//  2. Once a domain escalates, it stays escalated. Memory is consulted before
//     scoring, so a site that needed a browser last week needs one today
//     regardless of which A/B variant happened to be served.
//  3. Every input to the decision, and the decision itself, is recorded in the
//     artifact. A bug report starts with "which tier answered and why", and
//     that question has to be answerable without re-running anything.
//
// The stability check asserts that the *tier* is stable separately from
// asserting that the content is stable, because those are different failures
// with different causes.
package escalate

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/qcoderx/sieve/internal/static"
)

// Tier is a rung of the ladder.
type Tier string

const (
	// TierFetch is a plain HTTP GET with static extraction. Sub-second.
	TierFetch Tier = "fetch"
	// TierRender loads the page in a browser and captures once after settle.
	TierRender Tier = "render"
	// TierSweep is the full checkpoint sweep.
	TierSweep Tier = "sweep"
	// TierRecover adds canvas recovery on top of the sweep.
	TierRecover Tier = "recover"
)

// Rank orders the tiers so they can be compared and clamped.
func (t Tier) Rank() int {
	switch t {
	case TierFetch:
		return 0
	case TierRender:
		return 1
	case TierSweep:
		return 2
	case TierRecover:
		return 3
	}
	return 0
}

// ParseTier converts a string, returning false when it names no tier.
func ParseTier(s string) (Tier, bool) {
	switch Tier(strings.ToLower(strings.TrimSpace(s))) {
	case TierFetch:
		return TierFetch, true
	case TierRender:
		return TierRender, true
	case TierSweep:
		return TierSweep, true
	case TierRecover:
		return TierRecover, true
	}
	return "", false
}

// Thresholds are pinned, not computed.
//
// Every number here is a constant that a maintainer can change deliberately and
// a user can read. Deriving any of them from the page under judgement would
// make the decision depend on the thing being decided, which is how a scorer
// starts oscillating on pages near the line.
type Thresholds struct {
	// EscalateAbove is the score at or above which a browser is used.
	EscalateAbove float64
	// SweepAbove is the score at or above which the full sweep is used rather
	// than a single post-settle capture.
	SweepAbove float64
	// RecoverAbove is the score at or above which canvas recovery runs.
	RecoverAbove float64

	// MinStaticChars is the amount of readable text below which static
	// extraction is treated as having failed outright, regardless of anything
	// else. A page with 200 characters is a shell.
	MinStaticChars int
	// ThinTextRatio is the text-to-markup ratio below which the served document
	// looks like an application shell.
	ThinTextRatio float64
	// CanvasViewportShare is the fraction of the viewport a canvas must cover
	// for recovery to be worth attempting.
	CanvasViewportShare float64
}

// DefaultThresholds is the pinned configuration.
func DefaultThresholds() Thresholds {
	return Thresholds{
		EscalateAbove:       0.35,
		SweepAbove:          0.55,
		RecoverAbove:        0.75,
		MinStaticChars:      400,
		ThinTextRatio:       0.012,
		CanvasViewportShare: 0.25,
	}
}

// Decision is the outcome, with everything that produced it.
type Decision struct {
	Tier  Tier
	Score float64
	// Reason is a human-readable account of the scoring, recorded in the
	// artifact so a caller never has to guess why a page was judged cheap.
	Reason string
	// Pinned reports that the tier came from per-domain memory rather than from
	// this page's score.
	Pinned bool
	// Factors is the itemised scoring, for `sieve doctor` and for tuning.
	Factors []Factor
}

// Factor is one contribution to the score.
type Factor struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight"`
	Note   string  `json:"note,omitempty"`
}

// Score judges a page from its served bytes.
//
// libWeight is the strongest animation/scroll library signal found, which is
// only available once a browser has run; on the first, static-only pass it is
// zero and the decision rests on the document alone.
func Score(sig static.Signals, libWeight float64, libName string, th Thresholds) Decision {
	var factors []Factor
	add := func(name string, value, weight float64, note string) {
		factors = append(factors, Factor{Name: name, Value: value, Weight: weight, Note: note})
	}

	// 1. Absolute text volume. This is the single most decisive signal: a page
	//    that served almost no text either needs a browser or has nothing to
	//    say, and both cases are resolved by trying one.
	textScore := 0.0
	switch {
	case sig.TextChars < th.MinStaticChars/4:
		textScore = 1.0
		add("text_volume", float64(sig.TextChars), 0.35,
			fmt.Sprintf("only %d characters of text in the served HTML: this is a shell", sig.TextChars))
	case sig.TextChars < th.MinStaticChars:
		textScore = 0.7
		add("text_volume", float64(sig.TextChars), 0.35,
			fmt.Sprintf("%d characters is below the %d-character floor for a readable page", sig.TextChars, th.MinStaticChars))
	case sig.TextChars < th.MinStaticChars*4:
		textScore = 0.3
		add("text_volume", float64(sig.TextChars), 0.35, "sparse but plausibly complete")
	default:
		add("text_volume", float64(sig.TextChars), 0.35, "substantial text served statically")
	}

	// 2. Text against markup. A documentation page is text with a little markup
	//    around it; an application shell is markup with no text.
	ratioScore := 0.0
	if sig.HTMLBytes > 0 {
		switch {
		case sig.TextRatio < th.ThinTextRatio/3:
			ratioScore = 1.0
		case sig.TextRatio < th.ThinTextRatio:
			ratioScore = 0.6
		case sig.TextRatio < th.ThinTextRatio*3:
			ratioScore = 0.2
		}
		add("text_ratio", round(sig.TextRatio, 5), 0.2,
			fmt.Sprintf("%.2f%% of the served bytes are readable text", sig.TextRatio*100))
	}

	// 3. Structural richness. A page that arrives with a real outline has
	//    already told us what it contains.
	structure := sig.Headings + sig.Paragraphs + sig.Landmarks
	structScore := 0.0
	switch {
	case structure == 0:
		structScore = 1.0
		add("structure", 0, 0.15, "no headings, paragraphs or landmarks in the served HTML")
	case structure < 5:
		structScore = 0.6
		add("structure", float64(structure), 0.15, "very little structure served")
	case structure < 15:
		structScore = 0.2
		add("structure", float64(structure), 0.15, "some structure served")
	default:
		add("structure", float64(structure), 0.15, "rich structure served")
	}

	// 4. Explicit statements by the page that it needs a browser.
	declaredScore := 0.0
	if sig.NoScriptWarning {
		declaredScore = 1.0
		add("noscript_warning", 1, 0.1, "the page itself says it requires JavaScript")
	} else if sig.HydrationBlob {
		declaredScore = 0.5
		add("hydration_blob", 1, 0.1, "a framework hydration payload is present, so the served HTML may be a shell")
	} else {
		add("declared", 0, 0.1, "")
	}

	// 5. Script weight. A megabyte of bundle around two kilobytes of text is
	//    the signature of a client-rendered site.
	scriptScore := 0.0
	if sig.TextChars > 0 {
		perChar := float64(sig.ScriptBytes) / float64(sig.TextChars)
		switch {
		case perChar > 400:
			scriptScore = 1.0
		case perChar > 120:
			scriptScore = 0.5
		}
		add("script_weight", round(perChar, 1), 0.1,
			fmt.Sprintf("%.0f bytes of JavaScript per character of text", perChar))
	}

	// 6. Canvas presence. Not evidence that rendering is needed for *text*, but
	//    decisive for whether recovery should run.
	canvasScore := 0.0
	if sig.CanvasElements > 0 {
		canvasScore = 1.0
		add("canvas", float64(sig.CanvasElements), 0.1,
			fmt.Sprintf("%d canvas element(s) in the served HTML", sig.CanvasElements))
	}

	// 6b. Visibility logic the static tier could not evaluate.
	//
	// The static extractor scans inline stylesheets for rules that hide
	// content, but it does not fetch external stylesheets and does not resolve
	// selectors with combinators. A page whose hiding logic is mostly out of
	// reach is one where tier 0's judgement about what is visible is weakest --
	// and getting that wrong means either ingesting hidden text or discarding
	// visible text. Rendering resolves it definitively, so unresolved
	// visibility logic is a reason to escalate rather than to guess.
	opaqueScore := 0.0
	if sig.ExternalStylesheets > 0 || sig.ComplexHidingRules > 0 {
		opaqueScore = 0.5
		if sig.ExternalStylesheets > 2 || sig.ComplexHidingRules > 20 {
			opaqueScore = 1.0
		}
		add("unresolved_visibility", float64(sig.ExternalStylesheets+sig.ComplexHidingRules), 0.08,
			fmt.Sprintf("%d external stylesheet(s) and %d hiding rule(s) the static tier cannot evaluate",
				sig.ExternalStylesheets, sig.ComplexHidingRules))
	}

	// 7. Library fingerprints, when a browser has already run.
	if libWeight > 0 {
		add("library", libWeight, 0.2, "detected "+libName)
	}

	score := textScore*0.35 + ratioScore*0.2 + structScore*0.15 +
		declaredScore*0.1 + scriptScore*0.1 + canvasScore*0.1 +
		opaqueScore*0.08 + libWeight*0.2
	if score > 1 {
		score = 1
	}
	score = round(score, 3)

	d := Decision{Score: score, Factors: factors}
	switch {
	case score >= th.RecoverAbove && sig.CanvasElements > 0:
		d.Tier = TierRecover
	case score >= th.SweepAbove:
		d.Tier = TierSweep
	case score >= th.EscalateAbove:
		d.Tier = TierRender
	default:
		d.Tier = TierFetch
	}

	// Some evidence is decisive on its own and must not be left to arithmetic.
	//
	// A page can serve a complete, well-structured article and still hide half
	// of it behind a scroll hijacker: Lenis and Locomotive mean content is
	// revealed by scrolling, not by loading, and no amount of served text
	// changes that. Weighting such a signal alongside the others let a
	// text-rich page score 0.2 with a confirmed hijacker present and stay at
	// tier 0, which is precisely the silent coverage loss the ladder exists to
	// avoid.
	//
	// So a strong library signal sets a floor on the tier rather than nudging a
	// number. The floor is stated here as a rule a reader can check, not buried
	// in a weight.
	if floor := libraryFloor(libWeight); floor.Rank() > d.Tier.Rank() {
		d.Tier = floor
		d.Reason = fmt.Sprintf("raised to %q because %s was detected: this library reveals or relocates "+
			"content during scrolling, so a static read would miss it regardless of how much text was served "+
			"(page otherwise scored %.3f)", floor, libName, score)
		return d
	}
	d.Reason = explain(d, th)
	return d
}

// libraryFloor maps a library signal onto the minimum tier it justifies.
func libraryFloor(w float64) Tier {
	switch {
	case w >= 0.85:
		// Scroll hijackers, WebGL renderers and text-splitting plugins. Every
		// one of these makes the served HTML an incomplete account of the page.
		return TierSweep
	case w >= 0.55:
		// Reveal-on-scroll and client-side routers: rendering is needed, but a
		// single capture after settle is usually enough.
		return TierRender
	}
	return TierFetch
}

func explain(d Decision, th Thresholds) string {
	var top []Factor
	for _, f := range d.Factors {
		if f.Note != "" && f.Value > 0 {
			top = append(top, f)
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].Weight > top[j].Weight })
	if len(top) > 3 {
		top = top[:3]
	}
	notes := make([]string, 0, len(top))
	for _, f := range top {
		notes = append(notes, f.Note)
	}
	if len(notes) == 0 {
		return fmt.Sprintf("score %.3f, below the %.2f escalation threshold", d.Score, th.EscalateAbove)
	}
	return fmt.Sprintf("score %.3f chose tier %q; %s", d.Score, d.Tier, strings.Join(notes, "; "))
}

// Memory records which tier a domain has needed before.
//
// This is the hysteresis that stops a page near the threshold from being judged
// differently on different days. A domain that has ever needed a browser keeps
// needing one: the cost of occasionally over-working a page is a few seconds,
// and the cost of a tool that wavers is the project's central claim.
//
// It only ever ratchets upward. There is no decay, because a site that used to
// need rendering and now does not is a rare event, and getting it wrong in that
// direction silently loses content.
type Memory struct {
	mu   sync.RWMutex
	seen map[string]Tier
}

// NewMemory builds an empty memory.
func NewMemory() *Memory {
	return &Memory{seen: map[string]Tier{}}
}

// Apply consults the memory and raises the decision if the domain has needed
// more before. It then records the outcome.
func (m *Memory) Apply(host string, d Decision) Decision {
	host = strings.ToLower(strings.TrimPrefix(host, "www."))
	if host == "" {
		return d
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if prev, ok := m.seen[host]; ok && prev.Rank() > d.Tier.Rank() {
		d.Reason = fmt.Sprintf("pinned to %q by an earlier run on this domain (this page scored %.3f, which would have chosen %q)",
			prev, d.Score, d.Tier)
		d.Tier = prev
		d.Pinned = true
	}
	if cur, ok := m.seen[host]; !ok || d.Tier.Rank() > cur.Rank() {
		m.seen[host] = d.Tier
	}
	return d
}

// Note records that a domain turned out to need a given tier, whether or not
// the scorer predicted it. The sweep calls this when it discovers a scroll
// hijacker or a canvas that static extraction could not have known about.
func (m *Memory) Note(host string, t Tier) {
	host = strings.ToLower(strings.TrimPrefix(host, "www."))
	if host == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.seen[host]; !ok || t.Rank() > cur.Rank() {
		m.seen[host] = t
	}
}

// Snapshot exports the memory so it can be persisted between runs. Hysteresis
// that only lasts for one process is not hysteresis.
func (m *Memory) Snapshot() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.seen))
	for k, v := range m.seen {
		out[k] = string(v)
	}
	return out
}

// Restore loads a persisted memory.
func (m *Memory) Restore(in map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range in {
		if t, ok := ParseTier(v); ok {
			if cur, exists := m.seen[k]; !exists || t.Rank() > cur.Rank() {
				m.seen[k] = t
			}
		}
	}
}

func round(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int(v*p+0.5)) / p
}
