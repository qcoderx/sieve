package static

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func folded(t *testing.T, doc string) (controls, chars int) {
	t.Helper()
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	return countFolded(root)
}

// prose is long enough to clear minFoldedRegion, because every case below is
// about *which* regions count rather than about the size floor.
const prose = `Firing takes four days and the kiln is then left sealed for a week ` +
	`to cool, because opening it early cracks the glaze. Anyone wanting work in a ` +
	`particular firing should deliver greenware a month beforehand.`

// TestFoldedContentCountsReadingMatterNotFurniture pins the three judgements
// that make this signal usable, each of which was chosen from measurement
// rather than taste.
//
// Counting every shut region on two hundred real sites sent 24.7% of them to a
// browser. Almost none of that was folded prose: it was closed mobile
// navigation, dropdown menus and decorative panels marked aria-hidden, which
// exist in the hundreds on a large site and hold a few characters each.
// mlb.com alone offered 287 shut regions holding seven characters apiece.
//
// With the three rules below the same corpus escalates 9.8%, and the pages that
// remain are ones where a browser plainly earns its cost: vodafone.co.uk folds
// 15,980 characters into three regions, polar.com folds 7,854 into one.
//
// If any of these assertions is ever relaxed, that ratio is what moves, and it
// moves in the direction of spending a browser on the majority of the web to
// open its own hamburger menu.
func TestFoldedContentCountsReadingMatterNotFurniture(t *testing.T) {
	t.Run("a closed details holding prose counts", func(t *testing.T) {
		c, n := folded(t, `<details><summary>Delivery</summary><p>`+prose+`</p></details>`)
		if c != 1 || n < minFoldedRegion {
			t.Errorf("got %d control(s) / %d chars, want one region of real size", c, n)
		}
	})

	t.Run("an open details is not folded", func(t *testing.T) {
		if c, _ := folded(t, `<details open><summary>Delivery</summary><p>`+prose+`</p></details>`); c != 0 {
			t.Errorf("got %d, want 0: the page is already showing this", c)
		}
	})

	t.Run("a menu is furniture however much link text it holds", func(t *testing.T) {
		// The shape that made this signal fire on a quarter of the web.
		var sb strings.Builder
		sb.WriteString(`<div aria-hidden="true">`)
		for i := 0; i < 40; i++ {
			sb.WriteString(`<a href="/x">Some navigation destination</a>`)
		}
		sb.WriteString(`</div>`)
		if c, n := folded(t, sb.String()); c != 0 {
			t.Errorf("got %d control(s) / %d chars, want 0: a folded menu is not folded reading matter", c, n)
		}
	})

	t.Run("a nav is furniture by declaration", func(t *testing.T) {
		if c, _ := folded(t, `<nav aria-hidden="true"><p>`+prose+`</p></nav>`); c != 0 {
			t.Errorf("got %d, want 0", c)
		}
	})

	t.Run("a region too small to hold a fact does not count", func(t *testing.T) {
		if c, _ := folded(t, `<details><summary>More</summary><p>Back to top.</p></details>`); c != 0 {
			t.Errorf("got %d, want 0: a browser spent on twelve characters buys nothing", c)
		}
	})

	t.Run("a panel named by a collapsed control counts", func(t *testing.T) {
		doc := `<button aria-expanded="false" aria-controls="p">Specification</button>` +
			`<div id="p"><p>` + prose + `</p></div>`
		if c, _ := folded(t, doc); c != 1 {
			t.Errorf("got %d, want 1", c)
		}
	})

	// The two lists have to agree. If this side argues for a browser on a
	// consent banner, sieve spends one on nearly every page on the web and is
	// refused by its own prober when it arrives.
	t.Run("a control the prober will refuse is not an argument for a browser", func(t *testing.T) {
		for _, label := range []string{
			"Accept all cookies", "I am over 18", "Add to cart", "Sign in", "Subscribe",
		} {
			doc := `<button aria-expanded="false" aria-controls="p">` + label + `</button>` +
				`<div id="p"><p>` + prose + `</p></div>`
			if c, _ := folded(t, doc); c != 0 {
				t.Errorf("%q: got %d, want 0; capture.js will decline to press this, so "+
					"counting it argues for a browser that arrives and does nothing", label, c)
			}
		}
	})

	t.Run("a folded region is charged once, not once per nesting", func(t *testing.T) {
		doc := `<div aria-hidden="true"><details><summary>x</summary><p>` + prose +
			`</p></details><p>` + prose + `</p></div>`
		if c, _ := folded(t, doc); c != 1 {
			t.Errorf("got %d, want 1: an accordion inside a shut tab is one shut thing", c)
		}
	})
}
