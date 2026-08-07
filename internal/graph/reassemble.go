package graph

import (
	"sort"
	"strconv"
	"strings"

	"github.com/qcoderx/sieve/internal/capture"
)

// run is a reassembled piece of text: one or more captured nodes that rendered
// as a single run on a single line.
type run struct {
	Text    string
	Nodes   []*capture.Node
	Block   string
	LineTop float64
	BBox    capture.Box
	// Lead is the first node in document order, which supplies the run's tag,
	// landmark, link target and checkpoint.
	Lead *capture.Node
	// MaxOpacity is the highest opacity any constituent node ever reached.
	MaxOpacity  float64
	EverVisible bool
	Fixed       bool
	// leadPad and trailPad record whitespace that surrounded the run's first
	// and last fragments in the source.
	leadPad  bool
	trailPad bool
}

// group is every run sharing one block-level container, in reading order.
type group struct {
	Block string
	Runs  []*run
	Lead  *capture.Node
}

// reassemble puts shattered text back together.
//
// Text-splitting plugins turn a headline into one element per word, or per
// character, each with its own box and its own computed style. Classified
// as-is, a 30-character headline becomes 30 blocks, each of them a
// single-letter "heading" set at 76px. Every downstream signal -- font size
// medians, heading clustering, reading order, token count -- is corrupted by
// it, and the artifact is unreadable.
//
// The reconstruction runs in three stages:
//
//  1. Group runs by the nearest block-level ancestor. Splitting plugins wrap
//     words and characters in inline-blocks, which are deliberately not treated
//     as block boundaries, so every fragment of one headline shares a group.
//  2. Within a group, split into rendered lines by vertical position, because a
//     line break is a real boundary that must survive as at least a space.
//  3. Within a line, order fragments left to right and join them, inserting a
//     space only where the gap between two fragments is wider than the letter
//     spacing that separates characters inside a word.
func reassemble(nodes []capture.Node) []*group {
	if len(nodes) == 0 {
		return nil
	}

	byBlock := make(map[string]*group, len(nodes)/4+1)
	order := make([]*group, 0, len(nodes)/4+1)
	for i := range nodes {
		n := &nodes[i]
		g, ok := byBlock[n.Block]
		if !ok {
			g = &group{Block: n.Block}
			byBlock[n.Block] = g
			order = append(order, g)
		}
		g.Runs = append(g.Runs, &run{
			Text: n.Text, Nodes: []*capture.Node{n}, Block: n.Block,
			LineTop: n.LineTop, BBox: n.BBox, Lead: n,
			MaxOpacity: n.MaxOpacity, EverVisible: n.EverVisible, Fixed: n.Fixed,
			leadPad: n.Pad&1 != 0, trailPad: n.Pad&2 != 0,
		})
	}

	for _, g := range order {
		g.Runs = mergeLines(g.Runs)
		if len(g.Runs) > 0 {
			g.Lead = g.Runs[0].Lead
		}
	}
	// Groups come out in the document order of their first fragment. The
	// ordering pass reorders them properly; this only makes the input
	// deterministic.
	sort.SliceStable(order, func(i, j int) bool {
		return comparePath(order[i].Block, order[j].Block) < 0
	})
	return order
}

// mergeLines collapses fragments into one run per rendered line.
func mergeLines(runs []*run) []*run {
	if len(runs) <= 1 {
		return runs
	}
	// Sort into rendered order: down the page, then across each line. Document
	// order is the tiebreaker so that two fragments at identical coordinates --
	// which happens with overlapping absolutely positioned text -- stay in the
	// order the author wrote them.
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.LineTop != b.LineTop {
			return a.LineTop < b.LineTop
		}
		if a.BBox.X() != b.BBox.X() {
			return a.BBox.X() < b.BBox.X()
		}
		return comparePath(a.Lead.Path, b.Lead.Path) < 0
	})

	out := make([]*run, 0, 4)
	cur := runs[0]
	lineRef := cur.LineTop
	for _, r := range runs[1:] {
		// Two fragments are on the same line when their reported line tops are
		// within a fraction of the font size. An exact match is too strict:
		// mixed font sizes on one line (a capital drop cap, a superscript)
		// report different tops for the same visual line.
		tol := 0.4 * maxf(r.Lead.FontSize, cur.Lead.FontSize)
		if tol < 3 {
			tol = 3
		}
		if absf(r.LineTop-lineRef) <= tol && sameTextStyle(cur.Lead, r.Lead) {
			joinRun(cur, r)
			continue
		}
		out = append(out, cur)
		cur = r
		lineRef = r.LineTop
	}
	out = append(out, cur)

	// A block's lines are then joined into one run, because a paragraph that
	// wraps over four lines is one paragraph. Line breaks inside a block become
	// spaces; the block boundary is what survives as a real break.
	if len(out) > 1 && sameBlockText(out) {
		merged := out[0]
		for _, r := range out[1:] {
			joinAcrossLines(merged, r)
		}
		return []*run{merged}
	}
	return out
}

// sameBlockText reports whether a set of lines should be treated as one
// continuous run of prose. Lines that differ in style -- a heading immediately
// followed by a caption inside the same container -- must stay separate.
func sameBlockText(lines []*run) bool {
	for i := 1; i < len(lines); i++ {
		if !sameTextStyle(lines[0].Lead, lines[i].Lead) {
			return false
		}
		// A link and its surrounding prose are separate runs even when they
		// share styling, because the link target belongs to only part of it.
		if lines[0].Lead.Href != lines[i].Lead.Href {
			return false
		}
	}
	return true
}

// sameTextStyle reports whether two fragments were set in the same type, and so
// could plausibly be parts of one run.
func sameTextStyle(a, b *capture.Node) bool {
	if a == nil || b == nil {
		return false
	}
	if absf(a.FontSize-b.FontSize) > 0.6 {
		return false
	}
	if a.Weight != b.Weight {
		return false
	}
	if a.Italic != b.Italic {
		return false
	}
	if a.Family != b.Family {
		return false
	}
	if a.Transform != b.Transform {
		return false
	}
	return true
}

// joinRun appends r to cur, deciding on a space from geometry.
func joinRun(cur, r *run) {
	sep := ""
	// The gap between two adjacent characters of one word is exactly the
	// letter spacing. The gap where a space glyph was is that plus roughly a
	// quarter of the font size. Anything above the midpoint is a word break.
	//
	// This is why letter spacing has to be in the threshold: a headline set at
	// 0.18em tracking has 0.18em between every pair of characters, and a fixed
	// threshold would insert a space between all of them.
	fs := maxf(r.Lead.FontSize, cur.Lead.FontSize)
	thresh := maxf(r.Lead.Tracking, 0) + 0.14*fs
	gap := r.BBox.X() - cur.BBox.Right()
	if gap > thresh {
		sep = " "
	}
	// Whitespace the source actually contained overrides the geometric guess.
	// An inline link sits flush against the sentence it interrupts, so the gap
	// between their boxes is zero either way; only the recorded padding can say
	// whether "Lisboa. hello@example.com" or "Lisboa.hello@example.com" is right.
	if cur.trailPad || r.leadPad {
		sep = " "
	}
	// A fragment that already ends or begins with a word boundary needs no help.
	if strings.HasSuffix(cur.Text, " ") || strings.HasPrefix(r.Text, " ") {
		sep = ""
	}
	cur.Text += sep + r.Text
	cur.Nodes = append(cur.Nodes, r.Nodes...)
	cur.BBox = unionBox(cur.BBox, r.BBox)
	absorb(cur, r)
}

// joinAcrossLines appends a wrapped continuation line. A line break always
// separates words, so a space is unconditional. Hyphenation is not undone: a
// browser's automatic hyphen is not in the text content, and a hard hyphen the
// author typed is meant to be there.
func joinAcrossLines(cur, r *run) {
	if cur.Text != "" && r.Text != "" &&
		!strings.HasSuffix(cur.Text, " ") && !strings.HasPrefix(r.Text, " ") {
		cur.Text += " "
	}
	cur.Text += r.Text
	cur.Nodes = append(cur.Nodes, r.Nodes...)
	cur.BBox = unionBox(cur.BBox, r.BBox)
	absorb(cur, r)
}

func absorb(cur, r *run) {
	// The merged run ends where its last fragment ends.
	cur.trailPad = r.trailPad
	if r.MaxOpacity > cur.MaxOpacity {
		cur.MaxOpacity = r.MaxOpacity
	}
	if r.EverVisible {
		cur.EverVisible = true
	}
	if r.Fixed {
		cur.Fixed = true
	}
	// The earliest checkpoint any fragment appeared at is the run's checkpoint:
	// a run exists from the moment its first part does.
	if r.Lead.Checkpoint < cur.Lead.Checkpoint {
		cur.Lead = r.Lead
	}
	if cur.Lead.Href == "" && r.Lead.Href != "" {
		cur.Lead.Href = r.Lead.Href
	}
}

func unionBox(a, b capture.Box) capture.Box {
	x0 := minf(a.X(), b.X())
	y0 := minf(a.Y(), b.Y())
	x1 := maxf(a.Right(), b.Right())
	y1 := maxf(a.Bottom(), b.Bottom())
	return capture.Box{x0, y0, x1 - x0, y1 - y0}
}

// comparePath orders two structural paths the way a document does.
//
// Plain string comparison is wrong: "div[10]" sorts before "div[2]", which
// would scramble any page with more than nine siblings anywhere. Segments are
// compared by tag first, then by index numerically.
func comparePath(a, b string) int {
	for {
		if a == b {
			return 0
		}
		if a == "" {
			return -1
		}
		if b == "" {
			return 1
		}
		sa, ra := nextSeg(a)
		sb, rb := nextSeg(b)
		if c := compareSeg(sa, sb); c != 0 {
			return c
		}
		a, b = ra, rb
	}
}

func nextSeg(p string) (seg, rest string) {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func compareSeg(a, b string) int {
	ta, ia := splitSeg(a)
	tb, ib := splitSeg(b)
	if ta != tb {
		if ta < tb {
			return -1
		}
		return 1
	}
	if ia != ib {
		if ia < ib {
			return -1
		}
		return 1
	}
	return 0
}

func splitSeg(s string) (tag string, idx int) {
	i := strings.IndexByte(s, '[')
	if i < 0 {
		return s, 0
	}
	j := strings.IndexByte(s[i:], ']')
	if j < 0 {
		return s[:i], 0
	}
	n, err := strconv.Atoi(s[i+1 : i+j])
	if err != nil {
		return s[:i], 0
	}
	return s[:i], n
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
