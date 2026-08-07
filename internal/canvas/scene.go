// Package canvas recovers meaning from content that exists only as pixels.
//
// A WebGL hero is invisible to every text parser: the words a visitor reads may
// be geometry in a 3D scene, or a texture, or a shader. Two attacks are worth
// making, in this order.
//
// The scene graph parse is cheap and exact. A .glb or .gltf file carries names
// for every node, mesh and material, and authors name things after what they
// are -- "Chair_Oak_Backrest", "Hero_Title_Furniture". When it works it costs a
// few microseconds and invents nothing.
//
// Vision captioning is the fallback, and it is the most expensive step in the
// whole pipeline by an order of magnitude. It runs only when a canvas is large
// enough to be carrying content and the scene graph yielded nothing.
//
// Everything either produces is tagged with its provenance and a confidence,
// because a consumer must always be able to tell recovered pixels from real
// text.
package canvas

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Scene is what a scene-graph file gave up.
type Scene struct {
	// Source is the asset URL.
	Source string
	// Generator is the tool that authored the file, from asset.generator.
	Generator string
	// Copyright is asset.copyright, which occasionally carries the real
	// attribution for a model.
	Copyright string
	// Names are the meaningful node, mesh and material names, deduplicated and
	// in the order they appeared.
	Names []string
	// Text is any string found in an `extras` block, where authors and
	// exporters put annotations, labels and descriptions.
	Text []string
	// Nodes and Meshes are raw counts, useful for judging how substantial the
	// scene is.
	Nodes  int
	Meshes int
}

// Empty reports whether the parse recovered nothing worth reporting.
func (s *Scene) Empty() bool {
	return s == nil || (len(s.Names) == 0 && len(s.Text) == 0 && s.Copyright == "")
}

// Summary renders the scene as a sentence for the content graph. It states
// plainly that these are names from a 3D file, because a reader must not
// mistake them for text that was on the page.
func (s *Scene) Summary() string {
	if s.Empty() {
		return ""
	}
	var parts []string
	if len(s.Names) > 0 {
		n := s.Names
		if len(n) > 24 {
			n = n[:24]
		}
		parts = append(parts, "3D scene contains: "+strings.Join(n, ", "))
	}
	for _, t := range s.Text {
		parts = append(parts, t)
	}
	if s.Copyright != "" {
		parts = append(parts, "Credited to "+s.Copyright)
	}
	return strings.Join(parts, ". ")
}

var (
	// ErrNotGLTF is returned when the bytes are not a scene-graph file.
	ErrNotGLTF = errors.New("not a glTF or GLB file")

	glbMagic  = [4]byte{'g', 'l', 'T', 'F'}
	chunkJSON = uint32(0x4E4F534A) // "JSON"
)

// maxSceneJSON bounds the JSON chunk. A glTF file is untrusted remote input;
// a 400MB JSON chunk in a 32MB file is a decompression-style attack, and the
// length field is attacker-controlled.
const maxSceneJSON = 64 << 20

// ParseAsset parses either a binary GLB or a JSON glTF.
func ParseAsset(url string, body []byte) (*Scene, error) {
	if len(body) == 0 {
		return nil, ErrNotGLTF
	}
	if len(body) >= 12 && body[0] == glbMagic[0] && body[1] == glbMagic[1] &&
		body[2] == glbMagic[2] && body[3] == glbMagic[3] {
		return parseGLB(url, body)
	}
	// A .gltf file is plain JSON.
	trimmed := strings.TrimLeftFunc(string(body[:min(len(body), 64)]), unicode.IsSpace)
	if strings.HasPrefix(trimmed, "{") {
		return parseGLTFJSON(url, body)
	}
	return nil, ErrNotGLTF
}

// parseGLB walks the GLB container: a 12-byte header followed by length-tagged
// chunks. Only the JSON chunk is of interest; the binary chunk holds vertex
// data with nothing readable in it.
func parseGLB(url string, body []byte) (*Scene, error) {
	if len(body) < 20 {
		return nil, ErrNotGLTF
	}
	total := binary.LittleEndian.Uint32(body[8:12])
	// The declared length must not exceed what we actually have. Trusting it
	// blindly is how a malformed file becomes an out-of-range panic.
	if int(total) > len(body) {
		total = uint32(len(body))
	}

	off := 12
	for off+8 <= int(total) {
		clen := binary.LittleEndian.Uint32(body[off : off+4])
		ctype := binary.LittleEndian.Uint32(body[off+4 : off+8])
		off += 8
		if clen > maxSceneJSON || off+int(clen) > len(body) {
			break
		}
		if ctype == chunkJSON {
			return parseGLTFJSON(url, body[off:off+int(clen)])
		}
		off += int(clen)
		// Chunks are 4-byte aligned.
		if pad := off % 4; pad != 0 {
			off += 4 - pad
		}
	}
	return nil, fmt.Errorf("%w: no JSON chunk", ErrNotGLTF)
}

// gltfDoc is the subset of the glTF schema that carries authored language.
// Geometry, animation and accessors are deliberately absent: they are numbers.
type gltfDoc struct {
	Asset struct {
		Generator string          `json:"generator"`
		Copyright string          `json:"copyright"`
		Extras    json.RawMessage `json:"extras"`
	} `json:"asset"`
	Scenes []struct {
		Name   string          `json:"name"`
		Extras json.RawMessage `json:"extras"`
	} `json:"scenes"`
	Nodes []struct {
		Name   string          `json:"name"`
		Extras json.RawMessage `json:"extras"`
	} `json:"nodes"`
	Meshes []struct {
		Name   string          `json:"name"`
		Extras json.RawMessage `json:"extras"`
	} `json:"meshes"`
	Materials []struct {
		Name string `json:"name"`
	} `json:"materials"`
	Images []struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	} `json:"images"`
	Extras json.RawMessage `json:"extras"`
}

func parseGLTFJSON(url string, data []byte) (*Scene, error) {
	if len(data) > maxSceneJSON {
		return nil, fmt.Errorf("%w: JSON chunk too large", ErrNotGLTF)
	}
	var doc gltfDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotGLTF, err)
	}

	s := &Scene{
		Source:    url,
		Generator: strings.TrimSpace(doc.Asset.Generator),
		Copyright: strings.TrimSpace(doc.Asset.Copyright),
		Nodes:     len(doc.Nodes),
		Meshes:    len(doc.Meshes),
	}

	seen := make(map[string]bool)
	add := func(n string) {
		n = cleanName(n)
		if n == "" || seen[strings.ToLower(n)] {
			return
		}
		seen[strings.ToLower(n)] = true
		s.Names = append(s.Names, n)
	}

	for _, sc := range doc.Scenes {
		add(sc.Name)
		collectExtras(sc.Extras, &s.Text)
	}
	for _, n := range doc.Nodes {
		add(n.Name)
		collectExtras(n.Extras, &s.Text)
	}
	for _, m := range doc.Meshes {
		add(m.Name)
		collectExtras(m.Extras, &s.Text)
	}
	for _, m := range doc.Materials {
		add(m.Name)
	}
	for _, im := range doc.Images {
		add(im.Name)
	}
	collectExtras(doc.Extras, &s.Text)
	collectExtras(doc.Asset.Extras, &s.Text)

	dedupeStrings(&s.Text)
	return s, nil
}

// collectExtras pulls human-readable strings out of an arbitrary `extras`
// value. glTF puts no schema on extras, so exporters and authors use it for
// anything: labels, descriptions, CMS ids, float arrays. Only strings that look
// like language are kept.
func collectExtras(raw json.RawMessage, out *[]string) {
	if len(raw) == 0 {
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	walkExtras(v, out, 0)
}

func walkExtras(v any, out *[]string, depth int) {
	if depth > 6 || len(*out) > 200 {
		return
	}
	switch t := v.(type) {
	case string:
		if s := meaningfulSentence(t); s != "" {
			*out = append(*out, s)
		}
	case []any:
		for _, e := range t {
			walkExtras(e, out, depth+1)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		// Deterministic order: the artifact hash must not depend on Go's map
		// iteration.
		sort.Strings(keys)
		for _, k := range keys {
			walkExtras(t[k], out, depth+1)
		}
	}
}

// meaningfulSentence keeps strings that read like language and rejects the
// identifiers, hashes and paths that dominate `extras` in practice.
func meaningfulSentence(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 8 || len(s) > 600 {
		return ""
	}
	if strings.ContainsAny(s, "{}<>") || strings.Contains(s, "://") {
		return ""
	}
	letters, spaces := 0, 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
		} else if r == ' ' {
			spaces++
		}
	}
	// Language has spaces and is mostly letters. A UUID has neither property.
	if spaces < 1 || letters*2 < len(s) {
		return ""
	}
	return s
}

// cleanName turns an authored node name into something readable, and rejects
// the ones that carry no information.
//
// Exporters emit an enormous volume of "Object_017", "mesh_3", "Cube.001",
// "Material.042". Those name nothing; including them would fill the artifact
// with noise and, worse, make a reader think the page said something it did
// not.
func cleanName(n string) string {
	n = strings.TrimSpace(n)
	if n == "" || len(n) > 120 {
		return ""
	}
	// Split on the separators modelling tools use, then drop pure-numeric and
	// single-character fragments.
	fields := strings.FieldsFunc(n, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '|' || r == ':'
	})
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		if isAllDigits(f) {
			continue
		}
		if len(f) == 1 && !unicode.IsLetter(rune(f[0])) {
			continue
		}
		kept = append(kept, f)
	}
	if len(kept) == 0 {
		return ""
	}
	out := strings.Join(kept, " ")
	// Split camelCase into words so "HeroTitleFurniture" reads as language.
	out = splitCamel(out)
	if len(out) < 3 {
		return ""
	}
	if isGenericName(out) {
		return ""
	}
	return out
}

var genericNames = map[string]bool{
	"object": true, "mesh": true, "cube": true, "sphere": true, "plane": true,
	"cylinder": true, "node": true, "scene": true, "material": true,
	"group": true, "empty": true, "root": true, "armature": true, "camera": true,
	"light": true, "circle": true, "cone": true, "torus": true, "default": true,
	"untitled": true, "collection": true, "geometry": true, "primitive": true,
	"lambert": true, "phong": true, "standard surface": true, "mat": true,
}

func isGenericName(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	if genericNames[l] {
		return true
	}
	// "Cube 001" reduces to "Cube" once the number is stripped, and is still
	// generic.
	if i := strings.IndexByte(l, ' '); i > 0 && genericNames[l[:i]] {
		rest := strings.TrimSpace(l[i+1:])
		return isAllDigits(rest) || genericNames[rest]
	}
	return false
}

func splitCamel(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			// Insert a break at a lower-to-upper transition, and at the end of
			// an acronym run ("HTMLParser" -> "HTML Parser").
			if unicode.IsLower(prev) || unicode.IsDigit(prev) ||
				(i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(prev)) {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func dedupeStrings(ss *[]string) {
	seen := make(map[string]bool, len(*ss))
	out := (*ss)[:0]
	for _, s := range *ss {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	*ss = out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
