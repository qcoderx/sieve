package capture

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Merged is the union of every checkpoint, with each distinct piece of content
// present exactly once.
type Merged struct {
	Nodes       []Node
	Latent      []LatentNode
	Actions     []Action
	Media       []Media
	Canvases    []Canvas
	Disclosures []Disclosure
	Meta        Meta

	Checkpoints   int
	DocHeight     float64
	ViewportW     float64
	ViewportH     float64
	FramesRead    int
	FramesBlocked int
	Truncated     bool
	// LatentTruncated reports that the hidden-content budget was exhausted, so
	// the latent tier is itself incomplete.
	LatentTruncated bool

	// ObservedVisibleChars is the largest number of readable characters the
	// browser had on screen at any single checkpoint, summed across the sweep
	// as new content appeared. It is the denominator for graph retention.
	ObservedVisibleChars int

	// NewPerCheckpoint records how many previously unseen nodes each checkpoint
	// contributed. The sweep uses it to decide when to stop, and the artifact
	// reports it because a long tail of zeros is evidence the sweep was
	// complete rather than merely budget-limited.
	NewPerCheckpoint []int
}

// Accumulator folds snapshots into a Merged as they arrive, so a sweep never
// holds more than one checkpoint's raw nodes in memory at a time. On a large
// site the raw capture is 30-50x the size of the deduplicated result.
type Accumulator struct {
	nodes []Node
	// byKey indexes on path+text, the exact-identity case.
	byKey map[uint64]int32
	// byText indexes on text alone, and only for text long enough that a
	// collision would be a genuine repeat rather than a coincidence. It catches
	// the case where a framework tears down and rebuilds a node, changing its
	// path while the content stays the same.
	byText map[uint64]int32

	latent    []LatentNode
	latentKey map[uint64]bool

	actions  []Action
	actKey   map[string]int32
	actDedup map[string]int32

	media   []Media
	medKey  map[string]int32
	canvas  []Canvas
	canvKey map[string]int32

	disclosures []Disclosure
	discKey     map[string]bool

	meta     Meta
	metaSet  bool
	newCount []int

	docHeight       float64
	viewportW       float64
	viewportH       float64
	framesRead      int
	framesBlocked   int
	truncated       bool
	latentTruncated bool
	checkpoints     int
	visibleChars    int
}

// minTextForFuzzyDedup is the rune count above which two identical strings are
// assumed to be the same content even at different paths. Short strings such as
// "Home", "01" or a repeated call to action legitimately occur many times on
// one page and must not collapse.
const minTextForFuzzyDedup = 32

func NewAccumulator() *Accumulator {
	return &Accumulator{
		byKey:     make(map[uint64]int32, 4096),
		byText:    make(map[uint64]int32, 2048),
		latentKey: make(map[uint64]bool, 1024),
		actKey:    make(map[string]int32, 256),
		actDedup:  make(map[string]int32, 256),
		medKey:    make(map[string]int32, 128),
		canvKey:   make(map[string]int32, 16),
		discKey:   make(map[string]bool, 64),
	}
}

// Add folds one checkpoint in and reports how many nodes were new.
func (a *Accumulator) Add(s *Snapshot) int {
	a.checkpoints++
	if s.DocHeight > a.docHeight {
		a.docHeight = s.DocHeight
	}
	if s.ViewportW > 0 {
		a.viewportW = s.ViewportW
	}
	if s.ViewportH > 0 {
		a.viewportH = s.ViewportH
	}
	if s.Frames > a.framesRead {
		a.framesRead = s.Frames
	}
	if s.FramesBlocked > a.framesBlocked {
		a.framesBlocked = s.FramesBlocked
	}
	if s.Truncated {
		a.truncated = true
	}
	if s.LatentTruncated {
		a.latentTruncated = true
	}
	if s.Meta != nil && !a.metaSet {
		a.meta = *s.Meta
		a.metaSet = true
	}

	fresh := 0
	for i := range s.Nodes {
		if a.addNode(&s.Nodes[i], s.Checkpoint) {
			fresh++
		}
	}
	for i := range s.Latent {
		a.addLatent(&s.Latent[i], s.Checkpoint)
	}
	for i := range s.Actions {
		a.addAction(&s.Actions[i])
	}
	for i := range s.MediaItems {
		a.addMedia(&s.MediaItems[i])
	}
	for i := range s.Canvases {
		a.addCanvas(&s.Canvases[i])
	}
	for i := range s.Disclosures {
		a.addDisclosure(&s.Disclosures[i])
	}
	a.newCount = append(a.newCount, fresh)
	return fresh
}

func (a *Accumulator) addNode(n *Node, checkpoint int) bool {
	if n.Text == "" {
		return false
	}
	key := hash2(n.Path, n.Text)
	long := utf8.RuneCountInString(n.Text) >= minTextForFuzzyDedup

	if idx, ok := a.byKey[key]; ok {
		a.reinforce(&a.nodes[idx], n)
		return false
	}
	if long {
		tk := hash1(n.Text)
		if idx, ok := a.byText[tk]; ok {
			a.reinforce(&a.nodes[idx], n)
			// Register the new path too, so the next checkpoint takes the fast
			// exact path instead of hashing text again.
			a.byKey[key] = idx
			return false
		}
	}

	cp := *n
	cp.Checkpoint = checkpoint
	cp.MaxOpacity = n.Opacity
	cp.EverVisible = n.Visible
	cp.Seen = 1
	a.countVisible(&cp)
	a.nodes = append(a.nodes, cp)
	idx := int32(len(a.nodes) - 1)
	a.byKey[key] = idx
	if long {
		a.byText[hash1(n.Text)] = idx
	}
	return true
}

// countVisible adds a run to the observed-text total the first time it is seen
// in a readable state, and reports whether it did.
//
// This total is the denominator for graph retention, so what counts as
// "observed" has to match what the graph is willing to emit: on screen, above
// the visible-opacity threshold, and not the same colour as its background.
func (a *Accumulator) countVisible(n *Node) bool {
	if n.CountedVisible {
		return true
	}
	if !n.Visible || n.Opacity <= MinVisibleOpacity || n.InvisibleColor {
		return false
	}
	a.visibleChars += utf8.RuneCountInString(n.Text)
	n.CountedVisible = true
	return true
}

// MinVisibleOpacity is the opacity a run must reach at some point in the sweep
// to count as content.
//
// It is defined here and consumed by the graph rather than duplicated, because
// the retention audit divides what the graph emitted by what the capture
// observed. Two thresholds that drifted apart would make that ratio compare
// different populations and quietly stop meaning anything.
//
// The value is not 1.0 because a great deal of legitimate body copy is set at
// 0.7 or 0.8 opacity as a design choice.
const MinVisibleOpacity = 0.12

// addLatent records hidden text once. A collapsed accordion is present in every
// checkpoint's capture, so without dedup the latent tier would grow linearly
// with the number of checkpoints.
func (a *Accumulator) addLatent(n *LatentNode, checkpoint int) {
	if n.Text == "" {
		return
	}
	key := hash2(n.Path, n.Text)
	if a.latentKey[key] {
		return
	}
	a.latentKey[key] = true
	cp := *n
	cp.Checkpoint = checkpoint
	a.latent = append(a.latent, cp)
}

// reinforce merges a repeat observation into the node already held.
//
// The geometry kept is the geometry from the checkpoint where the node was most
// fully revealed. Scroll-driven sites animate elements in from a transformed
// position, so the box measured at first sighting is usually 100-400px away
// from where the element actually comes to rest, and reading order computed
// from that box is wrong.
func (a *Accumulator) reinforce(dst *Node, n *Node) {
	dst.Seen++
	if n.Opacity > dst.MaxOpacity {
		dst.MaxOpacity = n.Opacity
	}
	if n.Visible {
		dst.EverVisible = true
	}
	// Most content is below the fold when it is first captured, and only
	// becomes visible several checkpoints later. Counting observed text only at
	// first sighting would therefore put almost the whole page outside the
	// denominator and report a retention figure far above 1 -- which is exactly
	// the kind of self-flattering number this audit exists not to produce.
	if !dst.CountedVisible {
		probe := *dst
		probe.Visible = n.Visible
		probe.Opacity = n.Opacity
		probe.InvisibleColor = n.InvisibleColor
		if a.countVisible(&probe) {
			dst.CountedVisible = true
		}
	}
	// One sighting at readable contrast is enough to clear the flag: a
	// cross-fade legitimately passes through a moment where text matches its
	// background.
	if !n.InvisibleColor {
		dst.InvisibleColor = false
	}
	// One sighting of a declared reveal is enough to establish it: the page
	// said so once, and a later checkpoint catching the element mid-transition
	// does not unsay it.
	if n.Revealable {
		dst.Revealable = true
	}
	better := n.Opacity > dst.Opacity || (n.Opacity >= dst.Opacity && n.Visible && !dst.Visible)
	if better {
		dst.Opacity = n.Opacity
		dst.Visible = n.Visible
		dst.BBox = n.BBox
		dst.LineTop = n.LineTop
		dst.FontSize = n.FontSize
		dst.Weight = n.Weight
		dst.Tracking = n.Tracking
		dst.LineHeight = n.LineHeight
		dst.Color = n.Color
	}
	if dst.Href == "" && n.Href != "" {
		dst.Href = n.Href
	}
	if dst.Landmark == "" && n.Landmark != "" {
		dst.Landmark = n.Landmark
	}
}

func (a *Accumulator) addAction(x *Action) {
	if idx, ok := a.actKey[x.Path]; ok {
		dst := &a.actions[idx]
		if dst.Label == "" && x.Label != "" {
			dst.Label = x.Label
		}
		if len(dst.Fields) == 0 && len(x.Fields) > 0 {
			dst.Fields = x.Fields
		}
		if x.BBox.Area() > dst.BBox.Area() {
			dst.BBox = x.BBox
		}
		return
	}
	// Identical destination and label reached from two paths is one action to a
	// reader, however many times the page renders it.
	sig := x.Kind + "\x00" + strings.ToLower(x.Label) + "\x00" + x.Href
	if x.Label != "" || x.Href != "" {
		if idx, ok := a.actDedup[sig]; ok {
			a.actKey[x.Path] = idx
			return
		}
	}
	a.actions = append(a.actions, *x)
	idx := int32(len(a.actions) - 1)
	a.actKey[x.Path] = idx
	if x.Label != "" || x.Href != "" {
		a.actDedup[sig] = idx
	}
}

func (a *Accumulator) addMedia(m *Media) {
	key := m.Src
	if key == "" {
		key = m.Path
	}
	if idx, ok := a.medKey[key]; ok {
		dst := &a.media[idx]
		if m.BBox.Area() > dst.BBox.Area() {
			dst.BBox = m.BBox
		}
		if dst.Alt == "" {
			dst.Alt = m.Alt
		}
		if dst.Caption == "" {
			dst.Caption = m.Caption
		}
		// One decorative sighting does not make it decorative; every sighting must agree.
		if !m.Decorative {
			dst.Decorative = false
		}
		return
	}
	a.media = append(a.media, *m)
	a.medKey[key] = int32(len(a.media) - 1)
}

func (a *Accumulator) addCanvas(c *Canvas) {
	if idx, ok := a.canvKey[c.Path]; ok {
		dst := &a.canvas[idx]
		if c.ViewportShare > dst.ViewportShare {
			dst.ViewportShare = c.ViewportShare
			dst.BBox = c.BBox
			dst.ViewBox = c.ViewBox
		}
		if dst.Context == "" {
			dst.Context = c.Context
		}
		if dst.Label == "" {
			dst.Label = c.Label
		}
		if dst.Fallback == "" {
			dst.Fallback = c.Fallback
		}
		return
	}
	a.canvas = append(a.canvas, *c)
	a.canvKey[c.Path] = int32(len(a.canvas) - 1)
}

func (a *Accumulator) addDisclosure(d *Disclosure) {
	if d.Label == "" {
		return
	}
	key := d.Kind + "\x00" + strings.ToLower(d.Label)
	if a.discKey[key] {
		return
	}
	a.discKey[key] = true
	a.disclosures = append(a.disclosures, *d)
}

// StableFor reports whether the last k checkpoints all added nothing new. This
// is the sweep's termination condition.
func (a *Accumulator) StableFor(k int) bool {
	if k <= 0 || len(a.newCount) < k {
		return false
	}
	for _, v := range a.newCount[len(a.newCount)-k:] {
		if v != 0 {
			return false
		}
	}
	return true
}

func (a *Accumulator) NodeCount() int { return len(a.nodes) }

// Result finalises the accumulation. Nodes come out in document order by
// first-sighting checkpoint, which gives the ordering pass a deterministic
// starting permutation regardless of map iteration.
func (a *Accumulator) Result() *Merged {
	nodes := a.nodes
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Checkpoint != nodes[j].Checkpoint {
			return nodes[i].Checkpoint < nodes[j].Checkpoint
		}
		return nodes[i].Path < nodes[j].Path
	})
	sort.SliceStable(a.latent, func(i, j int) bool {
		return a.latent[i].Path < a.latent[j].Path
	})
	sort.SliceStable(a.media, func(i, j int) bool {
		if a.media[i].BBox.Y() != a.media[j].BBox.Y() {
			return a.media[i].BBox.Y() < a.media[j].BBox.Y()
		}
		return a.media[i].BBox.X() < a.media[j].BBox.X()
	})
	return &Merged{
		Nodes:                nodes,
		Latent:               a.latent,
		Actions:              a.actions,
		Media:                a.media,
		Canvases:             a.canvas,
		Disclosures:          a.disclosures,
		Meta:                 a.meta,
		Checkpoints:          a.checkpoints,
		DocHeight:            a.docHeight,
		ViewportW:            a.viewportW,
		ViewportH:            a.viewportH,
		FramesRead:           a.framesRead,
		FramesBlocked:        a.framesBlocked,
		Truncated:            a.truncated,
		LatentTruncated:      a.latentTruncated,
		ObservedVisibleChars: a.visibleChars,
		NewPerCheckpoint:     a.newCount,
	}
}

// FNV-1a. Inlined rather than taken from hash/fnv so that hashing a pair of
// strings needs no allocation, no io.Writer and no interface dispatch. The
// sweep hashes several million strings on a large site.
const (
	fnvOffset uint64 = 14695981039346656037
	fnvPrime  uint64 = 1099511628211
)

func hash1(s string) uint64 {
	h := fnvOffset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

func hash2(a, b string) uint64 {
	h := fnvOffset
	for i := 0; i < len(a); i++ {
		h ^= uint64(a[i])
		h *= fnvPrime
	}
	h ^= 0
	h *= fnvPrime
	for i := 0; i < len(b); i++ {
		h ^= uint64(b[i])
		h *= fnvPrime
	}
	return h
}
