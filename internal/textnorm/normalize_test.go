package textnorm

import (
	"strings"
	"testing"
)

func TestStripsBidiOverrides(t *testing.T) {
	// A right-to-left override reverses how a run displays, so the text a human
	// proofreads and the text a model receives can differ. That is not a
	// theoretical concern; it is a documented technique.
	in := "‮reversed‬ and normal"
	got := Clean(in)

	if strings.ContainsRune(got.Text, 0x202E) || strings.ContainsRune(got.Text, 0x202C) {
		t.Errorf("bidi override survived: %q", got.Text)
	}
	if !got.HadBidi {
		t.Error("the report did not flag that a bidi override was present")
	}
	if !strings.Contains(got.Text, "reversed") || !strings.Contains(got.Text, "and normal") {
		t.Errorf("the visible words were removed along with the control: %q", got.Text)
	}
}

func TestStripsInvisibleCharacters(t *testing.T) {
	// Zero-width characters split a word so it survives a human read and a
	// substring search while reaching the model as separate tokens.
	in := "pass​word and a soft­hyphen and a tag\U000E0041char"
	got := Clean(in)

	for _, r := range got.Text {
		switch r {
		case 0x200B, 0x00AD:
			t.Errorf("invisible character survived: %q", got.Text)
		}
		if r >= 0xE0000 && r <= 0xE007F {
			t.Errorf("tag character survived: %q", got.Text)
		}
	}
	if !got.HadInvisible {
		t.Error("the report did not flag invisible characters")
	}
	if !strings.Contains(got.Text, "password") {
		t.Errorf("removing the zero-width space should rejoin the word: %q", got.Text)
	}
}

func TestCollapsesWhitespace(t *testing.T) {
	cases := map[string]string{
		"  leading and trailing  ":   "leading and trailing",
		"multiple\t\tspaces":          "multiple spaces",
		"line\nbreaks\nbecome spaces": "line breaks become spaces",
		"non breaking":           "non breaking",
		"ideographic　space":      "ideographic space",
	}
	for in, want := range cases {
		if got := CleanString(in); got != want {
			t.Errorf("CleanString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	// The fast path must not allocate or alter anything for the overwhelming
	// majority of runs, which contain nothing to strip.
	for _, s := range []string{
		"A studio pottery in Leeds.",
		"European oak, quarter-sawn, air-dried for four years",
		"Café — naïve résumé, 100% façade",
		"日本語のテキストもそのまま",
	} {
		got := Clean(s)
		if got.Text != s {
			t.Errorf("ordinary text was altered:\n  in  %q\n  out %q", s, got.Text)
		}
		if got.Removed != 0 || got.HadBidi || got.HadInvisible {
			t.Errorf("ordinary text was flagged: %+v", got)
		}
	}
}

func TestTruncateRespectsRuneBoundaries(t *testing.T) {
	// A cap that splits a multi-byte rune produces mojibake in every consumer.
	s := "日本語のテキストです"
	got, cut := Truncate(s, 5)
	if !cut {
		t.Error("Truncate should report that it cut")
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("truncation split a rune: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation did not mark itself: %q", got)
	}

	short := "brief"
	if out, cut := Truncate(short, 50); cut || out != short {
		t.Errorf("Truncate altered a string shorter than the cap: %q", out)
	}
}

func TestVersionIsPartOfTheContract(t *testing.T) {
	// Changing what this package does changes every artifact's content hash and
	// invalidates every cache everywhere. That has to be a decision, not an
	// accident, so the version is an input to the hash. If this constant moves,
	// the move should be deliberate and noted in the changelog.
	if Version != 1 {
		t.Logf("normalizer version is now %d — every cached artifact is invalidated. "+
			"Confirm this was intended and note it in the changelog.", Version)
	}
}
