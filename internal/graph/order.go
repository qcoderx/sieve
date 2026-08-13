package graph

import (
	"sort"
)

// orderResult is the reading order plus an honest account of how sure we are.
type orderResult struct {
	Order      []int // indices into the input, in reading order
	Confidence float64
	Basis      string // "geometry" or "checkpoint"
	// Agreement is how closely the two independent orderings agree.
	//
	// Geometry and first-appearance are computed from entirely different
	// evidence -- where a block sits, versus when it first showed up in the
	// sweep -- and on a well-behaved page they agree almost completely.
	// Divergence is precisely where reading order goes wrong, and since both
	// orderings exist anyway, the comparison costs one sort.
	Agreement float64
}

// orderBoxes computes reading order for a set of candidates.
//
// DOM order is not reading order on a design-led site. Pinned sections,
// transforms and grid placement routinely put the element that reads third
// first in the markup, and CSS `order` on a flex container reverses runs of
// items outright. Geometry is what a reader actually follows, so geometry is
// what the order is computed from.
//
// The algorithm is a recursive XY-cut. At each step it looks for a horizontal
// band of whitespace crossing the whole group; if one exists, the group splits
// there and each part is solved independently, top to bottom. If no horizontal
// cut exists -- which is exactly what a multi-column layout looks like, since
// the columns interleave vertically -- it looks for a vertical gap instead and
// splits into columns, solved left to right.
//
// That is what stops a two-column section from being read as alternating lines
// from each column, which is the single most common way extracted text becomes
// nonsense.
func orderBoxes(cands []*candidate, pageWidth float64) orderResult {
	if len(cands) == 0 {
		return orderResult{Confidence: 1, Basis: "geometry"}
	}

	// Decide whether document coordinates mean anything on this page.
	//
	// A site using a scroll-hijacking library translates content with a
	// transform while the document itself never scrolls, so every block reports
	// a Y within one viewport of every other. Ordering by Y there produces
	// noise. The sequence in which content first appeared during the sweep is
	// the better signal in that case, and saying so in the artifact is more
	// useful than pretending the geometry was sound.
	basis := "geometry"
	if !geometrySpread(cands) {
		basis = "checkpoint"
	}

	st := &cutState{cands: cands, pageWidth: pageWidth, basis: basis}
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	if basis == "checkpoint" {
		st.fallbackSort(idx)
		return orderResult{Order: idx, Confidence: 0.55, Basis: basis, Agreement: 1}
	}

	out := make([]int, 0, len(idx))
	st.cut(idx, 0, &out)

	// The second, independent ordering: the sequence in which the sweep first
	// saw each block. It uses none of the geometry the cut relies on.
	appearance := make([]int, len(cands))
	for i := range appearance {
		appearance[i] = i
	}
	sort.SliceStable(appearance, func(i, j int) bool {
		a, b := cands[appearance[i]], cands[appearance[j]]
		if a.Checkpoint != b.Checkpoint {
			return a.Checkpoint < b.Checkpoint
		}
		return comparePath(a.Path, b.Path) < 0
	})
	agreement := rankAgreement(out, appearance)

	conf := 1.0
	if st.ambiguous > 0 {
		// Every block resolved by a plain sort inside a group that could not be
		// cut is a block whose position is a guess.
		conf -= 0.5 * float64(st.ambiguous) / float64(len(cands))
	}
	// Two methods that disagree are two methods at most one of which is right.
	// The penalty is deliberately gentle: a page with pinned sections has
	// genuine, correct divergence between where a block sits and when it
	// appeared, so disagreement lowers confidence rather than condemning it.
	if agreement < 0.9 {
		conf -= (0.9 - agreement) * 0.5
	}
	if conf < 0.3 {
		conf = 0.3
	}
	return orderResult{
		Order:      out,
		Confidence: roundTo(conf, 0.01),
		Basis:      basis,
		Agreement:  roundTo(agreement, 0.01),
	}
}

// rankAgreement measures how similarly two orderings arrange the same items,
// as the fraction of pairs they place in the same relative order.
//
// A full Kendall tau would be O(n^2) or need a merge-sort trick; sampling pairs
// gives the same number to two decimal places for a fraction of the work, and
// two decimal places is all a confidence bucket needs. Sampling is
// deterministic -- a fixed stride, not a random draw -- because an audit figure
// that changes between identical runs is worse than no audit figure.
func rankAgreement(a, b []int) float64 {
	n := len(a)
	if n < 2 {
		return 1
	}
	posB := make([]int, n)
	for rank, item := range b {
		if item >= 0 && item < n {
			posB[item] = rank
		}
	}
	posA := make([]int, n)
	for rank, item := range a {
		if item >= 0 && item < n {
			posA[item] = rank
		}
	}

	// Compare each item against a handful of others at fixed offsets. Every
	// item participates, so a systematic disagreement in one region cannot hide.
	offsets := []int{1, 2, 3, 5, 8, 13, 21, 34}
	agree, total := 0, 0
	for i := 0; i < n; i++ {
		for _, off := range offsets {
			j := i + off
			if j >= n {
				break
			}
			total++
			if (posA[i] < posA[j]) == (posB[i] < posB[j]) {
				agree++
			}
		}
	}
	if total == 0 {
		return 1
	}
	return float64(agree) / float64(total)
}

type cutState struct {
	cands     []*candidate
	pageWidth float64
	basis     string
	ambiguous int
}

const maxCutDepth = 24

// cut solves one group and appends its indices to out in reading order.
func (s *cutState) cut(idx []int, depth int, out *[]int) {
	if len(idx) == 0 {
		return
	}
	if len(idx) == 1 || depth >= maxCutDepth {
		if len(idx) > 3 {
			s.ambiguous += len(idx)
		}
		s.fallbackSort(idx)
		*out = append(*out, idx...)
		return
	}

	rows, rowGap := s.splitY(idx)
	cols, colGap := s.splitX(idx)

	// Choosing the axis is the whole difficulty, and getting it wrong is what
	// produces the classic garbled two-column extraction.
	//
	// A textbook XY-cut always cuts horizontally first. That is correct only
	// when the horizontal band is the real boundary. Two columns of prose whose
	// paragraphs happen to line up also admit a horizontal cut -- straight
	// through both columns -- and taking it reads one paragraph from the left,
	// then one from the right, then back to the left. The text is all present
	// and completely incoherent.
	//
	// So the axes compete rather than being ranked in advance.
	if len(cols) > 1 && preferColumns(s, idx, cols, colGap, rowGap) {
		for _, g := range cols {
			s.cut(g, depth+1, out)
		}
		return
	}
	if len(rows) > 1 {
		for _, g := range rows {
			s.cut(g, depth+1, out)
		}
		return
	}
	if len(cols) > 1 {
		for _, g := range cols {
			s.cut(g, depth+1, out)
		}
		return
	}

	if len(idx) > 3 {
		s.ambiguous += len(idx)
	}
	s.fallbackSort(idx)
	*out = append(*out, idx...)
}

// interval is one box's extent on a single axis.
type interval struct {
	lo, hi float64
	idx    int
}

// preferColumns decides whether a vertical split describes the layout better
// than a horizontal one.
//
// The distinguishing property of a column is that it runs the height of the
// region: two columns of prose each span the whole section, whereas two boxes
// that merely sit side by side in one row do not. Requiring that, plus a gutter
// at least as wide as the widest horizontal band, separates real columns from
// coincidental alignment.
func preferColumns(s *cutState, idx []int, cols [][]int, colGap, rowGap float64) bool {
	// Table cells are the one case where reading across really is right, and a
	// table's columns pass every other test here.
	cells := 0
	for _, k := range idx {
		if s.cands[k].Type == TypeTable {
			cells++
		}
	}
	if cells*2 > len(idx) {
		return false
	}

	lo, hi := s.cands[idx[0]].BBox.Y(), s.cands[idx[0]].BBox.Bottom()
	for _, k := range idx {
		b := s.cands[k].BBox
		lo = minf(lo, b.Y())
		hi = maxf(hi, b.Bottom())
	}
	total := hi - lo
	if total <= 0 {
		return false
	}

	tall := 0
	for _, g := range cols {
		clo, chi := s.cands[g[0]].BBox.Y(), s.cands[g[0]].BBox.Bottom()
		for _, k := range g {
			b := s.cands[k].BBox
			clo = minf(clo, b.Y())
			chi = maxf(chi, b.Bottom())
		}
		if (chi-clo)/total >= 0.55 && len(g) > 1 {
			tall++
		}
	}
	if tall < 2 {
		return false
	}
	// A gutter narrower than the gaps between paragraphs is not a gutter.
	return colGap >= rowGap
}

// splitY partitions by bands of whitespace that cross the whole group, and
// reports the widest such band.
func (s *cutState) splitY(idx []int) ([][]int, float64) {
	iv := make([]interval, len(idx))
	for i, k := range idx {
		b := s.cands[k].BBox
		iv[i] = interval{b.Y(), b.Bottom(), k}
	}
	// Any gap at all is a real boundary vertically: consecutive paragraphs are
	// separated by margin, and splitting them is correct.
	return partition(iv, 0.5)
}

// splitX partitions by vertical gutters, and reports the widest one.
func (s *cutState) splitX(idx []int) ([][]int, float64) {
	iv := make([]interval, len(idx))
	for i, k := range idx {
		b := s.cands[k].BBox
		iv[i] = interval{b.X(), b.Right(), k}
	}
	// Horizontally the threshold has to be substantial. Words and inline
	// fragments have small gaps between them all the time, and treating those
	// as column gutters would shred a single line into "columns" of one word.
	// A real gutter is a meaningful share of the page width.
	minGap := s.pageWidth * 0.03
	if minGap < 24 {
		minGap = 24
	}
	groups, gap := partition(iv, minGap)
	if len(groups) < 2 {
		return groups, gap
	}
	// A column has to be a column, not one stray floated caption. Requiring
	// more than one box in at least two parts stops a single pull-quote sitting
	// beside a paragraph from splitting the section in two.
	multi := 0
	for _, g := range groups {
		if len(g) > 1 {
			multi++
		}
	}
	if multi < 2 {
		return nil, 0
	}
	return groups, gap
}

// partition sorts intervals and splits at the most significant breaks in
// coverage. Returned groups preserve the axis order; the second return is the
// width of the gap that was cut on.
//
// Cutting at *every* gap at once is the obvious implementation and it is wrong.
// Consider a section heading above two columns of prose. The gap below the
// heading and the gaps between the paragraph rows are all real, so a
// split-everywhere pass separates the heading and slices the columns into rows
// in the same step -- and once the rows are separate groups, there is no longer
// any column structure left to recognise. The text then reads across the
// columns instead of down them.
//
// Cutting only at the widest break, and at breaks close to it, preserves the
// hierarchy: the heading comes away first, and the columns are still intact
// when the next level looks at them. A run of evenly spaced paragraphs still
// separates in one step, because there all the gaps are the widest.
func partition(iv []interval, minGap float64) ([][]int, float64) {
	if len(iv) < 2 {
		return [][]int{indicesOf(iv)}, 0
	}
	sort.Slice(iv, func(i, j int) bool {
		if iv[i].lo != iv[j].lo {
			return iv[i].lo < iv[j].lo
		}
		return iv[i].hi < iv[j].hi
	})

	// First pass: find the widest break in coverage.
	var widest float64
	reach := iv[0].hi
	for _, v := range iv[1:] {
		if gap := v.lo - reach; gap > widest {
			widest = gap
		}
		if v.hi > reach {
			reach = v.hi
		}
	}
	if widest < minGap {
		return [][]int{indicesOf(iv)}, 0
	}

	// Second pass: cut at breaks comparable to the widest one. The absolute
	// slack keeps sub-pixel layout rounding from turning one boundary into two.
	cutAt := maxf(widest*0.9, widest-4)
	if cutAt < minGap {
		cutAt = minGap
	}

	var groups [][]int
	cur := []int{iv[0].idx}
	reach = iv[0].hi
	for _, v := range iv[1:] {
		if v.lo-reach >= cutAt {
			groups = append(groups, cur)
			cur = []int{v.idx}
			reach = v.hi
			continue
		}
		cur = append(cur, v.idx)
		if v.hi > reach {
			reach = v.hi
		}
	}
	groups = append(groups, cur)
	return groups, widest
}

func indicesOf(iv []interval) []int {
	out := make([]int, len(iv))
	for i, v := range iv {
		out[i] = v.idx
	}
	return out
}

// fallbackSort is used where the cut found no clean boundary: order by the
// checkpoint the content first appeared at, then down, then across.
func (s *cutState) fallbackSort(idx []int) {
	sort.SliceStable(idx, func(i, j int) bool {
		a, b := s.cands[idx[i]], s.cands[idx[j]]
		if s.basis == "checkpoint" && a.Checkpoint != b.Checkpoint {
			return a.Checkpoint < b.Checkpoint
		}
		// Boxes on the same visual line sort across, not down. Comparing raw
		// tops would order a tall box before a short one beside it purely
		// because its box starts a few pixels higher.
		if !sameLine(a, b) {
			return a.BBox.Y() < b.BBox.Y()
		}
		if a.BBox.X() != b.BBox.X() {
			return a.BBox.X() < b.BBox.X()
		}
		return comparePath(a.Path, b.Path) < 0
	})
}

func sameLine(a, b *candidate) bool {
	tol := 0.5 * maxf(a.Style.FontSize, b.Style.FontSize)
	if tol < 6 {
		tol = 6
	}
	return absf(a.BBox.CenterY()-b.BBox.CenterY()) <= tol
}

// geometrySpread reports whether document coordinates are informative.
//
// On a normally scrolling page, content is spread down a document far taller
// than one viewport. On a page whose scrolling is faked with transforms, every
// block reports a position inside the first screen. The difference is stark
// enough that a simple extent test separates them.
func geometrySpread(cands []*candidate) bool {
	if len(cands) < 8 {
		return true // too little to judge; geometry is the safer default
	}
	minY, maxY := cands[0].BBox.Y(), cands[0].BBox.Bottom()
	maxCP := 0
	for _, c := range cands {
		if c.BBox.Y() < minY {
			minY = c.BBox.Y()
		}
		if c.BBox.Bottom() > maxY {
			maxY = c.BBox.Bottom()
		}
		if c.Checkpoint > maxCP {
			maxCP = c.Checkpoint
		}
	}
	extent := maxY - minY
	// If the sweep travelled through many checkpoints but the content all sits
	// within about a screen and a half, the document was not really scrolling.
	if maxCP >= 3 && extent < 1500 {
		return false
	}
	return true
}
