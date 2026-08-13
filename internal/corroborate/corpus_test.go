package corroborate

import (
	"strings"
	"testing"
)

func TestConfirmsRealRecoveries(t *testing.T) {
	ix := New(0)
	ix.AddText("inline", `{"hero":{"headline":"Furniture that outlives its owner","sub":"Lisbon, since 1974"}}`)

	// A headline recovered from a 3D scene, matching what the site shipped.
	if !ix.Contains("Furniture that outlives its owner") {
		t.Error("a headline present in the shipped payload was not confirmed")
	}
	// Case, curly quotes and non-breaking spaces differ constantly between a
	// rendered headline and a JSON field. They must not defeat the match.
	if !ix.Contains("FURNITURE THAT OUTLIVES ITS OWNER") {
		t.Error("case difference defeated confirmation")
	}
	if !ix.Contains("Furniture that outlives its owner") {
		t.Error("non-breaking spaces defeated confirmation")
	}
}

func TestRejectsInvention(t *testing.T) {
	ix := New(0)
	ix.AddText("inline", "A studio pottery in Leeds. Wheel-thrown and wood-fired.")

	// This is what a vision model inventing atmosphere looks like.
	if ix.Contains("A serene minimalist workspace bathed in golden afternoon light") {
		t.Error("an invented description was confirmed; the cross-check is not working")
	}
}

func TestShortStringsCannotConfirm(t *testing.T) {
	ix := New(0)
	ix.AddText("script", "const NAV = ['Home','Work','About','Contact'];")

	// "Home" appears in every bundle ever written. Confirming on it would
	// promote noise to verified, which is worse than leaving it speculative.
	for _, s := range []string{"Home", "Work", "About"} {
		if ix.Contains(s) {
			t.Errorf("%q was confirmed; short strings match by accident", s)
		}
	}
}

func TestPartialConfirmation(t *testing.T) {
	ix := New(0)
	ix.AddText("api", "Every piece is made to order. A dining table takes nine weeks.")

	// A vision model rarely reproduces a headline verbatim; it commonly gets a
	// clause right and the rest approximate. Matching on the longest clause is
	// the difference between confirming most real recoveries and confirming
	// none of them.
	frag, ok := ix.ContainsAny("Every piece is made to order, roughly speaking")
	if !ok {
		t.Fatal("a recovery whose first clause matches exactly was not confirmed")
	}
	if !strings.Contains(strings.ToLower(frag), "made to order") {
		t.Errorf("confirmation reported the wrong fragment: %q", frag)
	}
}

func TestScriptLiteralExtraction(t *testing.T) {
	ix := New(0)
	ix.AddScript("script", `
		// A comment mentioning the quick brown fox should not be indexed.
		var a = "e3b0c44298fc1c149afbf4c8996fb924";
		var b = "flex items-center justify-between";
		var c = "The most quietly confident furniture coming out of Portugal";
		var d = 'https://example.com/assets/main.js';
		var e = ` + "`" + `Every piece is made to order` + "`" + `;
	`)

	if !ix.Contains("The most quietly confident furniture coming out of Portugal") {
		t.Error("a prose literal in a bundle was not indexed")
	}
	if !ix.Contains("Every piece is made to order") {
		t.Error("a template literal was not indexed")
	}
	// A hash has no spaces and is rejected before it can dilute the index.
	if ix.Contains("e3b0c44298fc1c149afbf4c8996fb924") {
		t.Error("a hex hash was indexed as prose")
	}
	// Comments are skipped: their prose was never shipped as content.
	if ix.Contains("the quick brown fox should not be indexed") {
		t.Error("a source comment was indexed")
	}
}

func TestIndexIsWriteOnly(t *testing.T) {
	// The index deliberately offers no way to read its contents back.
	//
	// That is the enforcement of the rule that intercepted payloads may only
	// confirm, never add: a hydration blob routinely carries draft copy, other
	// locales and unpublished records, and any API that returned them would
	// eventually be used to. Size and saturation are the only observable
	// properties, and neither reveals a word.
	ix := New(0)
	ix.AddText("inline", "unpublished draft: the autumn collection launches in October")

	if ix.Size() == 0 {
		t.Error("Size should report that something was indexed")
	}
	if len(ix.Sources()) != 1 {
		t.Errorf("Sources should report the input kinds, got %v", ix.Sources())
	}
	// If a future refactor adds an enumeration method, this test will not catch
	// it -- but the comment above says why it must not, and the type has no
	// such method today.
}

func TestSaturation(t *testing.T) {
	ix := New(256)
	for i := 0; i < 100; i++ {
		ix.AddText("script", strings.Repeat("a phrase with spaces ", 4))
	}
	if !ix.Saturated() {
		t.Error("the index should report saturation once its cap is reached")
	}
	// Saturation matters because it makes a negative result weaker evidence,
	// and the artifact says so rather than pretending otherwise.
	if ix.Size() > 300 {
		t.Errorf("the cap was not respected: %d bytes", ix.Size())
	}
}
