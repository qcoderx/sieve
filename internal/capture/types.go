// Package capture defines the wire format produced by the in-page extraction
// script and the merge logic that folds a sequence of scroll checkpoints into a
// single deduplicated set of nodes.
//
// The JSON field names are deliberately one or two characters. A single
// checkpoint on a heavy site can carry tens of thousands of nodes, and the
// payload crosses the CDP boundary as a string; short keys cut that transfer
// by roughly a third with no loss of meaning.
package capture

// Box is a rectangle in document space: x, y, width, height. Document space
// means the page origin, not the viewport, so boxes are directly comparable
// across checkpoints.
type Box [4]float64

func (b Box) X() float64      { return b[0] }
func (b Box) Y() float64      { return b[1] }
func (b Box) W() float64      { return b[2] }
func (b Box) H() float64      { return b[3] }
func (b Box) Right() float64  { return b[0] + b[2] }
func (b Box) Bottom() float64 { return b[1] + b[3] }
func (b Box) Area() float64   { return b[2] * b[3] }

// CenterY is the vertical midpoint, used by the ordering pass so that two runs
// on the same visual line sort together even when their heights differ.
func (b Box) CenterY() float64 { return b[1] + b[3]/2 }

// Node is one text-bearing element: an element that owns at least one non-empty
// direct child text node. Elements that only contain other elements are not
// captured, which is what keeps the node count proportional to visible text
// rather than to DOM size.
type Node struct {
	Path  string `json:"p"`  // stable structural path, e.g. "html/body/div[2]/p[0]"
	Block string `json:"bp"` // path of the nearest block-level ancestor
	Tag   string `json:"t"`  // lowercase tag name
	Text  string `json:"x"`  // whitespace-normalised own text

	Role string `json:"r,omitempty"`  // explicit ARIA role
	Aria string `json:"al,omitempty"` // aria-label, when it differs from text
	// Landmark is the nearest landmark ancestor: nav, header, footer, main,
	// aside, form, dialog.
	//
	// <header> and <footer> are resolved during the walk rather than reported
	// raw. A document has at most one banner and one contentinfo, so only the
	// first page-level header and the last page-level footer are landmarks; the
	// rest are section furniture, which is content. See countPageLandmarks.
	Landmark string `json:"lm,omitempty"`
	Href     string `json:"h,omitempty"` // resolved href when the node is inside a link

	FontSize   float64 `json:"fs"`           // px
	Weight     int     `json:"fw"`           // 100..900, normalised from keywords
	Tracking   float64 `json:"ls"`           // letter-spacing in px, 0 for "normal"
	LineHeight float64 `json:"lh,omitempty"` // px
	Family     string  `json:"ff,omitempty"` // first font family only
	Transform  string  `json:"tt,omitempty"` // text-transform, empty when "none"
	Color      string  `json:"c,omitempty"`  // rgb/rgba as serialised by the engine
	Italic     bool    `json:"it,omitempty"`

	// Opacity is the product of the element's own opacity and every ancestor's,
	// computed on the way down the tree. Scroll-reveal animations run this from
	// 0 to 1, so the maximum observed across checkpoints is the signal that
	// matters, not the value at any one checkpoint.
	Opacity float64 `json:"o"`
	// Visible reports whether the element was inside the viewport with a
	// non-degenerate box, a visible `visibility` chain and no `display:none`
	// ancestor at this checkpoint.
	Visible bool `json:"v"`
	// InvisibleColor marks text whose colour is indistinguishable from what is
	// behind it. Opacity and visibility are the two hiding techniques the
	// capture already sees; matching the text colour to the background is the
	// third, and it defeats both.
	InvisibleColor bool `json:"iv,omitempty"`

	// Pad records whitespace that surrounded this fragment in the source before
	// normalisation: bit 1 for leading, bit 2 for trailing. It matters only for
	// fragments of mixed content, where an inline link abuts the text beside it
	// and geometry alone cannot say whether a space was written between them.
	Pad int `json:"pd,omitempty"`

	// Revealable marks a run that is not currently legible but whose element or
	// an ancestor declares a transition or animation that would make it so.
	//
	// It is the page's own statement of intent, read from computed style: an
	// author writes `transition: opacity .8s` on a section because the section
	// is meant to appear. Text hidden in order to stay hidden carries no such
	// declaration. The flag never promotes anything by itself; it lets the graph
	// tell "waiting to be revealed" apart from "hidden", which are the same
	// thing to an opacity threshold and very different things to a reader.
	Revealable bool `json:"rv,omitempty"`

	// Fixed marks a node inside a position:fixed or position:sticky subtree.
	// Its BBox is in viewport coordinates, not document coordinates, because a
	// pinned element has no single document position. It is also the strongest
	// single signal of chrome that the page gives us.
	Fixed bool `json:"fx,omitempty"`

	BBox Box `json:"bb"`
	// LineTop is the rounded top edge of the element's first client rect. Runs
	// that share a block ancestor and a LineTop sat on the same rendered line,
	// which is how a heading shattered into per-character spans is put back
	// together.
	LineTop float64 `json:"lt"`
	// LineLeft is the left edge of that same first client rect: where the run
	// starts reading, as opposed to how far it extends.
	//
	// The two differ whenever a run wraps. BBox is the union of every line, so
	// its left edge is the left margin as soon as any line but the first begins
	// there -- which makes a wrapped run sort as though it started at the
	// margin, ahead of everything that really precedes it on its own first
	// line. Ordering by this instead is what keeps an inline link inside the
	// sentence it interrupts.
	LineLeft float64 `json:"lx"`

	// Depth is the element's depth in the composed tree, used as a tiebreaker
	// when two runs occupy the same geometry.
	Depth int `json:"d"`

	// The fields below are filled in by the sweep, not by the page. They are
	// still serialised: a snapshot has to round-trip them or a replayed capture
	// arrives with MaxOpacity of zero, every run looks as though it was never
	// visible, and the rebuilt graph is empty. The in-page script never sends
	// them, so they simply stay zero on a live capture.

	// Checkpoint records the first checkpoint at which this node was seen.
	Checkpoint int `json:"cp,omitempty"`
	// MaxOpacity, MinOpacity and EverVisible accumulate across checkpoints
	// during merge.
	//
	// MinOpacity exists to catch a reveal in the act. A run that was seen at
	// zero and later at one was animated into view while sieve was watching,
	// and that is direct evidence -- not a declaration, not a guess -- that this
	// page brings its text in by animation. It is the only signal that works on
	// a page whose reveals are driven from JavaScript, where nothing in the
	// computed style says anything is going to happen.
	MaxOpacity  float64 `json:"mo,omitempty"`
	MinOpacity  float64 `json:"mio,omitempty"`
	EverVisible bool    `json:"ev,omitempty"`
	// Seen counts the checkpoints this node appeared in.
	Seen int `json:"sn,omitempty"`
	// CountedVisible records that this run has already been added to the
	// observed-text total, so a run visible across ten checkpoints is counted
	// once.
	CountedVisible bool `json:"cv,omitempty"`
}

// LatentNode is text that exists in the document but was never rendered.
//
// This is the quarantine. It holds exactly the material the visibility filter
// exists to exclude, because that material is not one thing: a collapsed
// accordion body is content a reader can reach with one click, and an
// off-screen instruction aimed at an AI agent is an attack. Both look identical
// to a walker, so neither is discarded and neither is mixed into the content
// tier. Nothing here ever reaches a default payload.
type LatentNode struct {
	Path  string `json:"p"`
	Block string `json:"bp"`
	Tag   string `json:"t"`
	Text  string `json:"x"`

	Role     string `json:"r,omitempty"`
	Landmark string `json:"lm,omitempty"`
	Href     string `json:"h,omitempty"`

	// Reason is why this text was not rendered: display-none today, with room
	// for further mechanisms.
	Reason string `json:"why"`
	// ControlLabel is the accessible name of the widget that would reveal this
	// content -- the tab, the accordion header, the <summary>. It is what lets
	// an artifact say "there is a section behind a tab labelled Pricing"
	// instead of silently omitting it.
	ControlLabel string `json:"cl,omitempty"`
	// ControlKind is tab, disclosure, details or control.
	ControlKind string `json:"ck,omitempty"`

	Depth int `json:"d"`

	// Checkpoint is filled in by the sweep.
	Checkpoint int `json:"-"`
}

// Disclosure is a widget that reveals hidden content.
type Disclosure struct {
	Label string `json:"l"`
	Kind  string `json:"k"` // tab | disclosure | details | control
	// Expanded is nil when the control does not declare aria-expanded.
	Expanded *bool `json:"e,omitempty"`
	Selected bool  `json:"s,omitempty"`
}

// Action is a link, button or form discovered in the page. The PRD treats this
// as first-class output: an agent that can read a page but cannot see that a
// contact form exists has only done half the job.
type Action struct {
	Path     string  `json:"p"`
	Kind     string  `json:"k"`           // link | button | form
	Label    string  `json:"l"`           // accessible name
	Href     string  `json:"h,omitempty"` // absolute URL for links, form action for forms
	Method   string  `json:"m,omitempty"` // GET | POST
	Fields   []Field `json:"f,omitempty"` // form controls
	BBox     Box     `json:"bb"`
	Landmark string  `json:"lm,omitempty"`
	Disabled bool    `json:"dis,omitempty"`
}

// Field is one form control.
type Field struct {
	Name     string   `json:"n"`
	Type     string   `json:"t"`
	Label    string   `json:"l,omitempty"`
	Required bool     `json:"r,omitempty"`
	Options  []string `json:"o,omitempty"`
	Pattern  string   `json:"pt,omitempty"`
}

// Media is an image, video or 3D model reference.
type Media struct {
	Path      string `json:"p"`
	Kind      string `json:"k"` // image | video | model
	Src       string `json:"s"`
	Alt       string `json:"a,omitempty"`
	AltCapped bool   `json:"ac,omitempty"` // alt text was truncated at the metadata cap
	Title     string `json:"ti,omitempty"`
	Caption   string `json:"cp,omitempty"`
	BBox      Box    `json:"bb"`
	// Decorative marks images the page itself flags as presentational
	// (role="presentation" or an explicitly empty alt).
	Decorative bool `json:"dec,omitempty"`
}

// Canvas is a canvas element and the evidence needed to decide whether it is
// worth spending a vision call on.
type Canvas struct {
	Path string `json:"p"`
	BBox Box    `json:"bb"`
	// ViewBox is the same rectangle in viewport coordinates, which is what a
	// clipped screenshot needs.
	ViewBox Box `json:"vb"`
	// ViewportShare is the fraction of the viewport this canvas covered at the
	// checkpoint where it was largest.
	ViewportShare float64 `json:"vs"`
	// Context is the rendering context the page acquired, when detectable:
	// webgl, webgl2, 2d, or empty.
	Context string `json:"cx,omitempty"`
	Label   string `json:"l,omitempty"` // aria-label or title, free text if present
	// Fallback is the canvas element's child content, which is what a screen
	// reader is given. A well-built WebGL site puts a real description there,
	// authored rather than inferred, and it is the cheapest recovery available.
	Fallback string `json:"fb,omitempty"`
	// Blank is set when the canvas rasterised to a single flat colour, which
	// means there is nothing for vision to describe.
	Blank bool `json:"bl,omitempty"`
}

// SceneIntrospection is what walking the live 3D scene produced. It catches
// procedurally built scenes that never loaded an asset file.
type SceneIntrospection struct {
	Names []string `json:"n"`
	Texts []string `json:"t"`
	// Runs are the text objects found in the scene, each with the words it was
	// built from, in the order the scene was assembled.
	//
	// Texts flattens the same material into a summary for the canvas recovery
	// tier. This keeps them separate, because a site that draws its whole body
	// copy into WebGL -- igloo.inc draws every paragraph as glyph geometry --
	// deserves its paragraphs as paragraphs rather than one welded blob.
	Runs []SceneRun `json:"r,omitempty"`

	// Observed counts how many times three.js has touched the devtools hook
	// sieve installs. Non-zero means three.js is on the page and running, which
	// is knowable long before it has finished building anything -- so an empty
	// scene with a non-zero count is a scene that is coming, and one with a
	// zero count is a page that has no three.js on it at all. Waiting is worth
	// it in the first case and is pure cost in the second.
	Observed int `json:"o,omitempty"`
}

// SceneRun is one text object from a 3D scene.
type SceneRun struct {
	Text string `json:"x"`
	Name string `json:"n,omitempty"`
}

// Meta carries page-level facts that only the page can report.
type Meta struct {
	Title       string            `json:"ti"`
	Lang        string            `json:"lg,omitempty"`
	Description string            `json:"de,omitempty"`
	Canonical   string            `json:"ca,omitempty"`
	URL         string            `json:"u"`
	OpenGraph   map[string]string `json:"og,omitempty"`
	// JSONLD is raw structured-data text. It never renders, so it is a pure
	// metadata channel and is never emitted as-is: only a whitelisted set of
	// schema.org fields is read out of it.
	JSONLD []string `json:"ld,omitempty"`
}

// Snapshot is one checkpoint's worth of observation.
type Snapshot struct {
	Checkpoint  int          `json:"n"`
	ScrollY     float64      `json:"sy"`
	DocHeight   float64      `json:"dh"`
	ViewportW   float64      `json:"vw"`
	ViewportH   float64      `json:"vh"`
	Nodes       []Node       `json:"nodes"`
	Latent      []LatentNode `json:"latent"`
	Actions     []Action     `json:"actions"`
	MediaItems  []Media      `json:"media"`
	Canvases    []Canvas     `json:"canvases"`
	Disclosures []Disclosure `json:"disc"`
	Meta        *Meta        `json:"meta,omitempty"`
	// VisibleChars is how many characters of readable text the browser had on
	// screen at this checkpoint. It is the denominator for the retention audit.
	VisibleChars int `json:"vc"`
	// Frames counts same-origin iframes that were descended into, and
	// FramesBlocked counts cross-origin ones that could not be.
	Frames        int `json:"fr"`
	FramesBlocked int `json:"frx"`
	// Truncated is set when the node budget was hit and the walk stopped early.
	Truncated bool `json:"tr,omitempty"`
	// LatentTruncated is the same for the latent budget.
	LatentTruncated bool `json:"ltr,omitempty"`
}
