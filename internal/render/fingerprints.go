package render

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// LibrarySpec is one detector for an animation, scroll or 3D library.
//
// These live in a data file rather than in code on purpose. The tail of
// animation-library behaviours is endless and each new one is discovered by a
// site breaking; a contributor should be able to add a detector with a fixture
// and a pull request against JSON, without touching Go and without waiting for
// a release.
type LibrarySpec struct {
	// Name is the identifier reported in the artifact.
	Name string `json:"n"`
	// Global is a dotted path on `window` whose existence proves the library is
	// loaded, e.g. "gsap.ScrollTrigger".
	Global string `json:"g,omitempty"`
	// Selector is a CSS selector that proves it, for libraries that leave a
	// marker in the DOM rather than a global.
	Selector string `json:"s,omitempty"`
	// Weight is how strongly this library predicts that a cheap fetch will miss
	// content, from 0 to 1. A scroll hijacker is decisive; a tooltip library
	// says nothing.
	Weight float64 `json:"w"`
	// Class groups detectors for reporting: scroll, reveal, 3d, text, router.
	Class string `json:"c"`
	// Note explains what breaks, and is surfaced in `sieve doctor`.
	Note string `json:"note,omitempty"`
}

//go:embed fingerprints.json
var fingerprintsJSON []byte

var (
	libSpecs   []LibrarySpec
	libSpecErr error
)

// LibrarySpecs returns the versioned detector set.
func LibrarySpecs() ([]LibrarySpec, error) {
	if libSpecs == nil && libSpecErr == nil {
		if err := json.Unmarshal(fingerprintsJSON, &libSpecs); err != nil {
			libSpecErr = fmt.Errorf("parse fingerprints.json: %w", err)
		}
	}
	return libSpecs, libSpecErr
}

// LibraryWeight returns the highest weight among the detected libraries, which
// is what the escalation scorer consumes. The maximum rather than the sum: one
// scroll hijacker is already decisive, and five reveal libraries are not five
// times as decisive.
func LibraryWeight(found []string) (float64, string) {
	specs, err := LibrarySpecs()
	if err != nil {
		return 0, ""
	}
	byName := make(map[string]LibrarySpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	var best float64
	var which string
	for _, n := range found {
		if s, ok := byName[n]; ok && s.Weight > best {
			best, which = s.Weight, n
		}
	}
	return best, which
}

// LibraryNotes returns the maintenance notes for the detected libraries.
func LibraryNotes(found []string) []string {
	specs, err := LibrarySpecs()
	if err != nil {
		return nil
	}
	byName := make(map[string]LibrarySpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	var out []string
	for _, n := range found {
		if s, ok := byName[n]; ok && s.Note != "" {
			out = append(out, s.Name+": "+s.Note)
		}
	}
	return out
}
