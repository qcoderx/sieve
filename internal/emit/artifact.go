package emit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qcoderx/sieve/internal/graph"
)

// Artifact is the set of files one distillation produces.
type Artifact struct {
	Dir   string
	Files []File
}

// File is one emitted file.
type File struct {
	Name  string
	Bytes []byte
}

// Names of the files in an artifact directory. Consumers depend on these, so
// they are constants rather than string literals scattered through the code.
const (
	FileContent  = "content.json"
	FileMarkdown = "index.md"
	FileHTML     = "index.html"
	FileManifest = "manifest.json"
	MediaDir     = "media"
)

// Build renders every format from the graph.
//
// The artifact byte count is computed after rendering and written back into the
// graph before content.json is serialised, so the stats an artifact reports
// about itself are the stats of the artifact as shipped.
func Build(g *graph.Graph) (*Artifact, error) {
	md := Markdown(g, DefaultMarkdownOptions())
	htm := HTML(g)

	// The artifact's own size is the Markdown rendering: that is what an agent
	// actually reads, and measuring the JSON instead would report a number no
	// consumer ever pays.
	g.Stats.ArtifactBytes = int64(len(md))

	manifest := BuildManifest(g)
	manifestBytes, err := marshalIndent(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	contentBytes, err := marshalIndent(g)
	if err != nil {
		return nil, fmt.Errorf("encode content: %w", err)
	}

	return &Artifact{
		Files: []File{
			{Name: FileManifest, Bytes: manifestBytes},
			{Name: FileContent, Bytes: contentBytes},
			{Name: FileMarkdown, Bytes: []byte(md)},
			{Name: FileHTML, Bytes: []byte(htm)},
		},
	}, nil
}

// Write puts the artifact on disk, replacing any previous contents of the
// directory's own files but leaving unrelated files alone.
func (a *Artifact) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	for _, f := range a.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(p, f.Bytes); err != nil {
			return fmt.Errorf("write %s: %w", f.Name, err)
		}
	}
	a.Dir = dir
	return nil
}

// AddMedia attaches a downloaded asset under media/.
func (a *Artifact) AddMedia(name string, body []byte) {
	a.Files = append(a.Files, File{Name: MediaDir + "/" + name, Bytes: body})
}

// writeAtomic writes through a temporary file and renames, so a reader never
// sees a half-written artifact and an interrupted run never leaves a corrupt
// one behind.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sieve-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows will not rename onto an existing file.
	_ = os.Remove(path)
	return os.Rename(tmpName, path)
}

// marshalIndent produces stable, readable JSON. Indentation is worth the bytes:
// artifacts get committed, diffed and reviewed by hand, and a single-line JSON
// document makes a one-word change look like a rewrite.
func marshalIndent(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetIndent("", "  ")
	// Escaping HTML inside JSON strings would turn every apostrophe and angle
	// bracket in the extracted text into a \u sequence, inflating the artifact
	// and making it unreadable, for no benefit: this JSON is never interpolated
	// into a page.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// LoadGraph reads a content.json back into a graph, which is what the serve and
// MCP layers do with a cached artifact.
func LoadGraph(dir string) (*graph.Graph, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileContent))
	if err != nil {
		return nil, err
	}
	var g graph.Graph
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileContent, err)
	}
	return &g, nil
}

// MediaFilename derives a stable, safe filename for a downloaded asset.
//
// It must be deterministic: the same source URL has to produce the same name on
// every run or the artifact hash would change for no reason. It must also never
// escape the media directory, since the name is derived from a remote URL.
func MediaFilename(id, src string) string {
	ext := ""
	clean := src
	if i := strings.IndexAny(clean, "?#"); i >= 0 {
		clean = clean[:i]
	}
	if i := strings.LastIndexByte(clean, '.'); i >= 0 && len(clean)-i <= 6 {
		ext = strings.ToLower(clean[i:])
		for _, c := range ext[1:] {
			if !isAlnum(byte(c)) {
				ext = ""
				break
			}
		}
	}
	if ext == "" {
		ext = ".bin"
	}
	return id + ext
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// SortedNames lists the artifact's files, for reporting.
func (a *Artifact) SortedNames() []string {
	out := make([]string, 0, len(a.Files))
	for _, f := range a.Files {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

// TotalBytes is the artifact's size on disk.
func (a *Artifact) TotalBytes() int64 {
	var n int64
	for _, f := range a.Files {
		n += int64(len(f.Bytes))
	}
	return n
}
