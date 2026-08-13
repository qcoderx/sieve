package graph_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/qcoderx/sieve/internal/graph"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// goldenEntry is one block, keyed by something insertion-stable.
//
// # Why the key is not the block id
//
// Block ids are sequential, so inserting one paragraph near the top of a page
// renumbers every block below it. A golden diff keyed on ids would then show
// two hundred changed lines for a one-line change, and a reviewer who sees that
// twice stops reading golden diffs -- which is the same as not having them.
//
// Keying on the block's structural position plus a hash of its text means an
// insertion shows up as exactly one added line. Extraction regressions are
// silent by nature, and a diff nobody reads catches nothing.
type goldenEntry struct {
	Key        string `json:"key"`
	Type       string `json:"type"`
	Level      int    `json:"level,omitempty"`
	Region     string `json:"region"`
	Source     string `json:"source"`
	Confidence string `json:"confidence"`
	Text       string `json:"text"`
}

type golden struct {
	URL        string        `json:"url"`
	Title      string        `json:"title"`
	Sections   []string      `json:"sections"`
	Blocks     []goldenEntry `json:"blocks"`
	Latent     []goldenEntry `json:"latent,omitempty"`
	Gaps       []string      `json:"gaps,omitempty"`
	Actions    []string      `json:"actions"`
	Structured []string      `json:"structured,omitempty"`
	Audit      goldenAudit   `json:"audit"`
}

// goldenAudit records only the audit fields that should be stable. Timings and
// exact scores move with machine speed and would make the corpus noisy.
type goldenAudit struct {
	OrderBasis        string `json:"order_basis"`
	OrderConfidence   string `json:"order_confidence"`
	HeadingConfidence string `json:"heading_confidence"`
	LatentCount       int    `json:"latent_count"`
}

func snapshotGraph(g *graph.Graph) golden {
	out := golden{URL: g.URL, Title: g.Title}

	for _, s := range g.Sections {
		out.Sections = append(out.Sections, fmt.Sprintf("L%d %s", s.Level, s.Title))
	}
	for _, b := range g.Blocks {
		out.Blocks = append(out.Blocks, goldenEntry{
			Key:        blockKey(b),
			Type:       string(b.Type),
			Level:      b.Level,
			Region:     string(b.Region),
			Source:     string(b.Source),
			Confidence: string(b.Confidence),
			Text:       b.Text,
		})
	}
	for _, l := range g.Latent {
		out.Latent = append(out.Latent, goldenEntry{
			Key:  "latent:" + l.ControlKind + ":" + l.ControlLabel + ":" + textHash(l.Text),
			Type: string(l.Type),
			Text: l.Text,
		})
	}
	for _, gp := range g.Gaps {
		out.Gaps = append(out.Gaps, gp.Kind+": "+gp.Label)
	}
	for _, a := range g.Actions {
		desc := a.Type + " " + a.Label
		if a.Type == "form" {
			var names []string
			for _, f := range a.Fields {
				req := ""
				if f.Required {
					req = "*"
				}
				names = append(names, f.Name+":"+f.Type+req)
			}
			desc += " [" + strings.Join(names, " ") + "] → " + a.Method
		}
		out.Actions = append(out.Actions, desc)
	}
	for _, f := range g.Structured {
		out.Structured = append(out.Structured, f.Type+"."+f.Field+"="+f.Value)
	}
	sort.Strings(out.Actions)
	sort.Strings(out.Gaps)
	sort.Strings(out.Structured)

	out.Audit = goldenAudit{
		OrderBasis:        g.Audit.OrderBasis,
		OrderConfidence:   string(g.Audit.OrderConfidence),
		HeadingConfidence: string(g.Audit.HeadingConfidence),
		LatentCount:       len(g.Latent),
	}
	return out
}

// blockKey identifies a block by what it is rather than where it sits in the
// sequence.
func blockKey(b graph.Block) string {
	return fmt.Sprintf("%s/%s/%s", b.Region, b.Type, textHash(b.Text))
}

func textHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// TestGolden compares extraction against a stored artifact.
//
// Extraction quality regressions are silent: nothing errors, nothing crashes,
// the artifact just quietly says less than it did last week. A diff against a
// stored result is the only cheap way to notice.
//
// Run with -update to accept a change, and read the diff before you do.
func TestGolden(t *testing.T) {
	for _, page := range []string{"immersive", "adversarial"} {
		t.Run(page, func(t *testing.T) {
			g := buildFixture(t, page+"/")
			got := snapshotGraph(g)

			path := filepath.Join("..", "..", "testdata", "golden", page+".json")
			gotJSON, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			gotJSON = append(gotJSON, '\n')

			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, gotJSON, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s", path)
				return
			}

			wantJSON, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no golden file at %s.\nRun: go test ./internal/graph -run TestGolden -update", path)
			}

			var want golden
			if err := json.Unmarshal(wantJSON, &want); err != nil {
				t.Fatal(err)
			}
			diffGolden(t, want, got)
		})
	}
}

// diffGolden reports differences in a form a reviewer can act on: what
// disappeared, what appeared, and what changed shape while keeping its text.
func diffGolden(t *testing.T, want, got golden) {
	t.Helper()

	if want.Title != got.Title {
		t.Errorf("title changed:\n  was %q\n  now %q", want.Title, got.Title)
	}

	wantBlocks := map[string]goldenEntry{}
	for _, e := range want.Blocks {
		wantBlocks[e.Key] = e
	}
	gotBlocks := map[string]goldenEntry{}
	for _, e := range got.Blocks {
		gotBlocks[e.Key] = e
	}

	for key, w := range wantBlocks {
		g, ok := gotBlocks[key]
		if !ok {
			t.Errorf("block disappeared: [%s/%s] %q", w.Region, w.Type, short(w.Text))
			continue
		}
		if g.Type != w.Type || g.Level != w.Level || g.Region != w.Region {
			t.Errorf("block reclassified: %q\n  was %s/%s L%d\n  now %s/%s L%d",
				short(w.Text), w.Region, w.Type, w.Level, g.Region, g.Type, g.Level)
		}
		if g.Confidence != w.Confidence {
			t.Errorf("confidence changed for %q: %s -> %s", short(w.Text), w.Confidence, g.Confidence)
		}
	}
	for key, g := range gotBlocks {
		if _, ok := wantBlocks[key]; !ok {
			t.Errorf("block appeared: [%s/%s] %q", g.Region, g.Type, short(g.Text))
		}
	}

	// Ordering is checked separately from membership: the same blocks in a
	// different order is a real regression and a membership diff would miss it.
	if len(want.Blocks) == len(got.Blocks) {
		for i := range want.Blocks {
			if want.Blocks[i].Key != got.Blocks[i].Key {
				t.Errorf("reading order changed at position %d:\n  was %q\n  now %q",
					i, short(want.Blocks[i].Text), short(got.Blocks[i].Text))
				break
			}
		}
	}

	diffStrings(t, "section", want.Sections, got.Sections)
	diffStrings(t, "action", want.Actions, got.Actions)
	diffStrings(t, "gap", want.Gaps, got.Gaps)
	diffStrings(t, "structured fact", want.Structured, got.Structured)

	if want.Audit != got.Audit {
		t.Errorf("audit changed:\n  was %+v\n  now %+v", want.Audit, got.Audit)
	}

	wantLatent := map[string]bool{}
	for _, e := range want.Latent {
		wantLatent[e.Key] = true
	}
	for _, e := range got.Latent {
		if !wantLatent[e.Key] {
			t.Errorf("latent block appeared: %q", short(e.Text))
		}
		delete(wantLatent, e.Key)
	}
	for k := range wantLatent {
		t.Errorf("latent block disappeared: %s", k)
	}
}

func diffStrings(t *testing.T, kind string, want, got []string) {
	t.Helper()
	w := map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		if !w[s] {
			t.Errorf("%s appeared: %s", kind, s)
		}
		delete(w, s)
	}
	for s := range w {
		t.Errorf("%s disappeared: %s", kind, s)
	}
}
