package escalate

import (
	"testing"

	"github.com/qcoderx/sieve/internal/static"
)

// documentationPage is what a blog, docs page or article looks like: text with
// a little markup around it.
func documentationPage() static.Signals {
	return static.Signals{
		HTMLBytes: 48_000, TextChars: 9_800, TextRatio: 9800.0 / 48000,
		Headings: 12, Paragraphs: 40, Landmarks: 3, Links: 30,
		ScriptBytes: 20_000,
	}
}

// applicationShell is what a client-rendered site serves: markup with no text.
func applicationShell() static.Signals {
	return static.Signals{
		HTMLBytes: 6_000, TextChars: 40, TextRatio: 40.0 / 6000,
		Headings: 0, Paragraphs: 0, Landmarks: 0,
		ScriptBytes: 1_400_000, HydrationBlob: true, NoScriptWarning: true,
	}
}

func TestScoreSeparatesCheapFromExpensive(t *testing.T) {
	th := DefaultThresholds()

	cheap := Score(documentationPage(), 0, "", th)
	if cheap.Tier != TierFetch {
		t.Errorf("a documentation page should be answered by tier 0, got %q at score %.3f\n  %s",
			cheap.Tier, cheap.Score, cheap.Reason)
	}

	heavy := Score(applicationShell(), 0, "", th)
	if heavy.Tier.Rank() < TierRender.Rank() {
		t.Errorf("an application shell should escalate, got %q at score %.3f\n  %s",
			heavy.Tier, heavy.Score, heavy.Reason)
	}

	// Every decision has to carry its reasoning. A bug report starts with
	// "which tier answered and why", and that must be answerable from the
	// artifact rather than by re-running anything.
	for _, d := range []Decision{cheap, heavy} {
		if d.Reason == "" {
			t.Error("a decision was recorded without any reasoning")
		}
		if len(d.Factors) == 0 {
			t.Error("a decision was recorded without its itemised factors")
		}
	}
}

func TestLibraryEvidenceCarriesTheDecision(t *testing.T) {
	th := DefaultThresholds()
	// A page that looks perfectly readable statically, but whose scroll is
	// hijacked. Nothing in the served HTML says so.
	sig := documentationPage()

	without := Score(sig, 0, "", th)
	with := Score(sig, 1.0, "lenis", th)

	if without.Tier != TierFetch {
		t.Fatalf("precondition: expected tier 0 without library evidence, got %q", without.Tier)
	}
	if with.Tier.Rank() <= without.Tier.Rank() {
		t.Errorf("a confirmed scroll hijacker should raise the tier on its own: %q -> %q (%.3f -> %.3f)",
			without.Tier, with.Tier, without.Score, with.Score)
	}
}

// TestMemoryPinsAndRatchets covers the hysteresis that stops a page near the
// threshold from being judged differently on different days.
func TestMemoryPinsAndRatchets(t *testing.T) {
	m := NewMemory()
	th := DefaultThresholds()

	// First visit: the page needs a sweep.
	heavy := Score(applicationShell(), 0, "", th)
	heavy = m.Apply("example.com", heavy)
	if heavy.Tier.Rank() < TierRender.Rank() {
		t.Fatalf("precondition: expected escalation, got %q", heavy.Tier)
	}

	// Second visit: the same domain serves a page that scores as cheap --
	// because an A/B test served a different variant, or a sub-page really is
	// lighter. Without hysteresis the tool would waver.
	cheap := Score(documentationPage(), 0, "", th)
	cheap = m.Apply("example.com", cheap)
	if cheap.Tier != heavy.Tier {
		t.Errorf("a domain that has escalated should stay escalated: %q became %q", heavy.Tier, cheap.Tier)
	}
	if !cheap.Pinned {
		t.Error("a pinned decision must say it was pinned, or the artifact misreports why it did what it did")
	}
	if !contains(cheap.Reason, "pinned") {
		t.Errorf("the pinned reason should say so plainly: %q", cheap.Reason)
	}

	// www is the same site.
	viaWWW := m.Apply("www.example.com", Score(documentationPage(), 0, "", th))
	if viaWWW.Tier != heavy.Tier {
		t.Errorf("www.example.com should share example.com's memory, got %q", viaWWW.Tier)
	}

	// Memory only ratchets up. A domain that once needed a browser keeps
	// needing one; getting that wrong in the other direction silently loses
	// content.
	m.Note("example.com", TierFetch)
	after := m.Apply("example.com", Score(documentationPage(), 0, "", th))
	if after.Tier != heavy.Tier {
		t.Errorf("memory must not ratchet down: %q became %q", heavy.Tier, after.Tier)
	}
}

func TestMemoryRoundTrips(t *testing.T) {
	// Hysteresis that only lasts for one process is not hysteresis.
	a := NewMemory()
	a.Note("example.com", TierSweep)
	a.Note("other.com", TierRender)

	b := NewMemory()
	b.Restore(a.Snapshot())

	d := b.Apply("example.com", Decision{Tier: TierFetch, Score: 0.1})
	if d.Tier != TierSweep {
		t.Errorf("restored memory did not pin: got %q", d.Tier)
	}
}

func TestThresholdsArePinnedNotDerived(t *testing.T) {
	// The same signals must produce the same decision every time. A threshold
	// derived from the page being judged could move under it, and a tool whose
	// judgement depends on the weather has no claim to determinism.
	th := DefaultThresholds()
	sig := applicationShell()
	first := Score(sig, 0, "", th)
	for i := 0; i < 50; i++ {
		got := Score(sig, 0, "", th)
		if got.Tier != first.Tier || got.Score != first.Score {
			t.Fatalf("scoring is not deterministic: run %d gave %q/%.5f, first gave %q/%.5f",
				i, got.Tier, got.Score, first.Tier, first.Score)
		}
	}
}

func TestTierParsing(t *testing.T) {
	for _, s := range []string{"fetch", "RENDER", " sweep ", "recover"} {
		if _, ok := ParseTier(s); !ok {
			t.Errorf("ParseTier(%q) failed", s)
		}
	}
	if _, ok := ParseTier("turbo"); ok {
		t.Error("ParseTier accepted a tier that does not exist")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestStaticShortfallEscalates covers a page that looks readable and is not.
//
// cuberto.com serves 3,335 characters that static extraction can read and 9,236
// that a tag-strip can see. The scorer read the first number as "substantial
// text served statically", stayed at tier 0, and returned 25 of 34 ground-truth
// facts; the browser returns all of them. Nothing in the old signal set could
// tell that page from one where three thousand characters is the whole
// document, because text volume alone says nothing about what was missed.
func TestStaticShortfallEscalates(t *testing.T) {
	base := static.Signals{
		HTMLBytes: 153806, TextChars: 3335, TextRatio: 0.0217,
		Headings: 14, Paragraphs: 6, Landmarks: 59, Links: 35,
	}
	th := DefaultThresholds()

	// Without the comparison, this page reads as fine and stays put.
	blind := base
	blind.MarkupChars = 0
	if d := Score(blind, 0, "", th); d.Tier != TierFetch {
		t.Fatalf("fixture no longer reproduces the case: tier %q at %.3f", d.Tier, d.Score)
	}

	// With it, the gap is large enough to be worth a browser.
	seen := base
	seen.MarkupChars = 9236
	d := Score(seen, 0, "", th)
	if d.Tier == TierFetch {
		t.Errorf("static read %d of %d characters and the page still stayed at tier 0 "+
			"(score %.3f); a third of the visible text was being dropped silently",
			seen.TextChars, seen.MarkupChars, d.Score)
	}
	var found bool
	for _, f := range d.Factors {
		if f.Name == "static_shortfall" {
			found = true
		}
	}
	if !found {
		t.Error("the decision does not record why it escalated")
	}
}

// TestOrdinaryPagesDoNotEscalateOnFurniture: every page drops some furniture on
// purpose, and a rule that fired on that would send the whole web to a browser.
func TestOrdinaryPagesDoNotEscalateOnFurniture(t *testing.T) {
	th := DefaultThresholds()
	// A documentation page: plenty of text, a normal amount of chrome the
	// extractor discards.
	sig := static.Signals{
		HTMLBytes: 42000, TextChars: 7000, MarkupChars: 8200, TextRatio: 0.166,
		Headings: 20, Paragraphs: 40, Landmarks: 6, Links: 90,
	}
	if d := Score(sig, 0, "", th); d.Tier != TierFetch {
		t.Errorf("an ordinary page escalated to %q at %.3f; dropping 1,200 characters "+
			"of navigation is not evidence of a broken read", d.Tier, d.Score)
	}
}
