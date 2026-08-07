package canvas

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/corroborate"
	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/llm"
	"github.com/qcoderx/sieve/internal/textnorm"
)

// Options configures recovery.
type Options struct {
	// EnableVision turns on the vision model. It is off by default, and that
	// default is a security property rather than a cost saving: with vision
	// disabled the artifact structurally cannot contain invented text.
	// Enabling it is an explicit, logged, budgeted choice.
	EnableVision bool
	// VisionModel and VisionBudget bound the expensive path.
	VisionModel  string
	VisionBudget int64
	// APIKey overrides the environment.
	APIKey string
	// ViewportShareGate is the fraction of the viewport a canvas must cover
	// before vision is worth spending on it.
	ViewportShareGate float64
	// MaxVisionCalls is a hard ceiling per job.
	MaxVisionCalls int
	// OCR is an optional external recogniser. OCR is near-free, deterministic,
	// and fails loudly -- garbage output is obviously garbage -- whereas vision
	// fails quietly and confidently. So it runs first when available.
	OCR OCRFunc
	// Logf receives progress lines.
	Logf func(format string, args ...any)
}

// OCRFunc reads text off an image. It returns empty when it recognises nothing.
type OCRFunc func(ctx context.Context, png []byte) (string, error)

// DefaultOptions returns settings with vision off.
func DefaultOptions() Options {
	return Options{
		EnableVision:      false,
		VisionModel:       llm.DefaultModel,
		VisionBudget:      200_000,
		ViewportShareGate: 0.25,
		MaxVisionCalls:    4,
	}
}

// Shot is a rasterised canvas region.
type Shot struct {
	PNG     []byte
	Uniform bool
	Share   float64
}

// Input is everything recovery has to work with.
type Input struct {
	Canvases []capture.Canvas
	// Assets are intercepted scene-graph files, keyed by URL.
	Assets map[string][]byte
	// Shots are rasterised canvas regions, keyed by canvas path.
	Shots map[string]Shot
	// Scene is what walking the live 3D scene produced.
	Scene *capture.SceneIntrospection
	// Corpus answers whether a recovered string appears in what the site
	// shipped. It is a membership oracle and nothing else: it cannot be
	// enumerated, and nothing in it can become content.
	Corpus *corroborate.Index
}

// Recovery is one canvas's worth of recovered meaning.
type Recovery struct {
	CanvasPath string
	Text       string
	Source     graph.Source
	Score      float64
	// Confirmed reports that the text was found in the payload the site
	// shipped. For a guess about pixels this is the difference between
	// evidence and invention.
	Confirmed bool
	// ConfirmedBy is the fragment that matched, for the audit trail.
	ConfirmedBy string
	BBox        [4]float64
}

// Recoverer runs the attacks in order.
type Recoverer struct {
	opts Options
	llm  *llm.Client
}

// NewRecoverer builds a recoverer.
func NewRecoverer(opts Options) *Recoverer {
	if opts.ViewportShareGate <= 0 {
		opts.ViewportShareGate = 0.25
	}
	if opts.MaxVisionCalls <= 0 {
		opts.MaxVisionCalls = 4
	}
	return &Recoverer{opts: opts}
}

func (r *Recoverer) logf(format string, args ...any) {
	if r.opts.Logf != nil {
		r.opts.Logf(format, args...)
	}
}

// Recover attempts each canvas, cheapest and most exact attack first.
//
// The order is not arbitrary. Each rung is both cheaper and more trustworthy
// than the one below it, so the first that succeeds is also the best answer
// available:
//
//	1. The canvas element's own accessibility fallback. Authored by the site,
//	   exact, free. Most tools ignore it entirely.
//	2. The live scene graph, walked in the page. Catches procedurally built
//	   scenes that never loaded an asset file.
//	3. Intercepted .glb / .gltf assets. Exact when the author named things.
//	4. OCR. Deterministic, near-free, and it fails loudly.
//	5. Vision. Expensive, slow, and it fails quietly and confidently -- which
//	   is why it is last and off by default.
//
// Anything from rungs 4 and 5 is a guess about pixels, so it is cross-checked
// against the text the site shipped before it is allowed to count.
func (r *Recoverer) Recover(ctx context.Context, in Input) ([]Recovery, error) {
	var out []Recovery
	visionCalls := 0

	// Deterministic order: the artifact must not depend on map iteration.
	canvases := append([]capture.Canvas(nil), in.Canvases...)
	sort.SliceStable(canvases, func(i, j int) bool { return canvases[i].Path < canvases[j].Path })

	// The scene walk is page-wide rather than per-canvas, so it is resolved
	// once and attributed to the largest canvas.
	sceneText := sceneSummary(in.Scene)

	for _, cv := range canvases {
		rec := Recovery{
			CanvasPath: cv.Path,
			BBox:       [4]float64{cv.BBox[0], cv.BBox[1], cv.BBox[2], cv.BBox[3]},
		}

		// -- Attack 1: the accessibility fallback ---------------------------
		if txt := cleanRecovered(cv.Fallback); txt != "" {
			rec.Text = txt
			rec.Source = graph.SourceCanvasFallback
			rec.Score = 0.98
			out = append(out, rec)
			r.logf("canvas %s: recovered from its accessibility fallback", cv.Path)
			continue
		}
		if txt := cleanRecovered(cv.Label); txt != "" {
			rec.Text = txt
			rec.Source = graph.SourceCanvasFallback
			rec.Score = 0.9
			out = append(out, rec)
			continue
		}

		// -- Attack 2: the live scene graph ---------------------------------
		if sceneText != "" {
			rec.Text = sceneText
			rec.Source = graph.SourceCanvasScene
			rec.Score = 0.8
			out = append(out, rec)
			r.logf("canvas %s: recovered from the live scene graph", cv.Path)
			sceneText = "" // attribute once
			continue
		}

		// -- Attack 3: intercepted scene-graph assets -----------------------
		if txt, src := r.fromAssets(in.Assets); txt != "" {
			rec.Text = txt
			rec.Source = graph.SourceCanvasScene
			rec.Score = 0.75
			out = append(out, rec)
			r.logf("canvas %s: recovered from scene asset %s", cv.Path, src)
			delete(in.Assets, src)
			continue
		}

		// Below this line every attack is a guess about pixels, and the gate
		// applies: a canvas too small to be carrying content is not worth
		// rasterising, and a flat one has nothing in it.
		shot, hasShot := in.Shots[cv.Path]
		if !hasShot || len(shot.PNG) == 0 {
			out = append(out, rec)
			continue
		}
		if cv.ViewportShare < r.opts.ViewportShareGate {
			r.logf("canvas %s: %.0f%% of viewport, below the %.0f%% gate; not rasterised",
				cv.Path, cv.ViewportShare*100, r.opts.ViewportShareGate*100)
			out = append(out, rec)
			continue
		}
		if shot.Uniform {
			r.logf("canvas %s: rasterised to a flat colour; nothing to describe", cv.Path)
			out = append(out, rec)
			continue
		}

		// -- Attack 4: OCR ---------------------------------------------------
		if r.opts.OCR != nil {
			if txt, err := r.opts.OCR(ctx, shot.PNG); err == nil {
				if cleaned := cleanRecovered(txt); cleaned != "" {
					rec.Text = cleaned
					rec.Source = graph.SourceCanvasOCR
					rec.Score = 0.6
					r.confirm(&rec, in.Corpus)
					out = append(out, rec)
					r.logf("canvas %s: OCR recovered %d characters (confirmed=%v)",
						cv.Path, len(cleaned), rec.Confirmed)
					continue
				}
			} else {
				r.logf("canvas %s: OCR failed: %v", cv.Path, err)
			}
		}

		// -- Attack 5: vision ------------------------------------------------
		if !r.opts.EnableVision {
			out = append(out, rec)
			continue
		}
		if visionCalls >= r.opts.MaxVisionCalls {
			r.logf("canvas %s: vision call ceiling of %d reached", cv.Path, r.opts.MaxVisionCalls)
			out = append(out, rec)
			continue
		}
		txt, err := r.describe(ctx, shot.PNG)
		visionCalls++
		if err != nil {
			r.logf("canvas %s: vision failed: %v", cv.Path, err)
			out = append(out, rec)
			continue
		}
		if txt == "" {
			out = append(out, rec)
			continue
		}
		rec.Text = txt
		rec.Source = graph.SourceCanvasVision
		rec.Score = 0.4
		r.confirm(&rec, in.Corpus)
		out = append(out, rec)
	}
	return out, nil
}

// confirm cross-checks a guess against the text the site shipped.
//
// This single step is what converts an open-ended hallucination problem into a
// bounded confidence-scoring one. Most canvas headlines exist somewhere in the
// payload before they become pixels, so finding the string there is strong
// evidence the recovery is real -- and the payload is never read for content,
// only asked whether it contains something we already have.
//
// A string that is not found stays speculative and is excluded from the default
// payload. That is the whole defence: with vision off, invented text cannot
// exist; with vision on, invented text cannot be delivered as if it were real.
func (r *Recoverer) confirm(rec *Recovery, ix *corroborate.Index) {
	if ix == nil {
		rec.Score *= 0.7
		return
	}
	frag, ok := ix.ContainsAny(rec.Text)
	if !ok {
		rec.Confirmed = false
		rec.Score *= 0.5
		return
	}
	rec.Confirmed = true
	rec.ConfirmedBy = frag
	// Promotion, not certainty: the string demonstrably exists in what the site
	// shipped, which is as close to proof as a pixel recovery gets.
	rec.Score = 0.9
}

// fromAssets parses intercepted scene-graph files.
func (r *Recoverer) fromAssets(assets map[string][]byte) (string, string) {
	urls := make([]string, 0, len(assets))
	for u := range assets {
		urls = append(urls, u)
	}
	sort.Strings(urls)

	for _, u := range urls {
		sc, err := ParseAsset(u, assets[u])
		if err != nil || sc.Empty() {
			continue
		}
		if txt := cleanRecovered(sc.Summary()); txt != "" {
			return txt, u
		}
	}
	return "", ""
}

// visionPrompt is deliberately narrow.
//
// A vision model asked to "describe this image" will write a paragraph of
// atmosphere, and atmosphere is exactly the kind of plausible invention the
// fidelity metric exists to catch. Asking only for text that is legibly present
// keeps the output checkable against the page's own payload, which is what the
// cross-check needs.
const visionPrompt = `This image is a region of a web page rendered to a canvas element.

Transcribe only the words that are legibly rendered in the image. Do not
describe the scene, the style, the colours, the mood, or anything you infer.
Do not guess at partially visible or blurred text.

If there is no legible text at all, reply with exactly: NO_TEXT

Otherwise reply with the words only, in reading order, one line per visual line.`

func (r *Recoverer) describe(ctx context.Context, png []byte) (string, error) {
	if r.llm == nil {
		c, err := llm.New(llm.Options{
			APIKey:    r.opts.APIKey,
			Model:     r.opts.VisionModel,
			MaxTokens: 1024,
			Budget:    r.opts.VisionBudget,
			// Transcription is a perception task, not a reasoning one. Low
			// effort is both cheaper and less likely to elaborate.
			Effort: "low",
		})
		if err != nil {
			return "", err
		}
		r.llm = c
	}

	b64 := base64.StdEncoding.EncodeToString(png)
	res, err := r.llm.Ask(ctx, visionPrompt, []llm.ContentBlock{
		llm.ImageBlock("image/png", b64),
		llm.TextBlock("Transcribe the legible text in this canvas region."),
	})
	if err != nil {
		return "", err
	}
	if res.Refused {
		return "", fmt.Errorf("vision request declined (%s)", res.RefusalCategory)
	}
	txt := strings.TrimSpace(res.Text)
	if txt == "" || strings.Contains(txt, "NO_TEXT") {
		return "", nil
	}
	return cleanRecovered(txt), nil
}

// sceneSummary renders a live scene-graph walk into a sentence, stating plainly
// that these are names from a 3D scene so a reader cannot mistake them for text
// that was on the page.
func sceneSummary(sc *capture.SceneIntrospection) string {
	if sc == nil {
		return ""
	}
	var names []string
	seen := map[string]bool{}
	for _, n := range sc.Names {
		clean := cleanName(n)
		if clean == "" || seen[strings.ToLower(clean)] {
			continue
		}
		seen[strings.ToLower(clean)] = true
		names = append(names, clean)
		if len(names) >= 24 {
			break
		}
	}

	var parts []string
	if len(names) > 0 {
		parts = append(parts, "3D scene contains: "+strings.Join(names, ", "))
	}
	for _, t := range sc.Texts {
		if s := meaningfulSentence(t); s != "" {
			parts = append(parts, s)
		}
		if len(parts) > 12 {
			break
		}
	}
	return strings.Join(parts, ". ")
}

// cleanRecovered normalises recovered text and rejects what is too short or too
// noisy to be worth an entry.
func cleanRecovered(s string) string {
	s = textnorm.CleanString(s)
	if utf8.RuneCountInString(s) < 3 {
		return ""
	}
	out, _ := textnorm.Truncate(s, 2000)
	return out
}
