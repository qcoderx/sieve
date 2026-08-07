// Package snapshot records a capture so a graph can be rebuilt without a
// browser, a network, or the site.
//
// # Why this matters more than it looks
//
// The problem space has an endless tail of browser quirks and animation-library
// behaviours, each one discovered by a site breaking. Two recent examples --
// secondary tabs never compositing and therefore starving requestAnimationFrame,
// and --in-process-gpu with SwiftShader killing frame production outright --
// appear in no documentation anywhere.
//
// Without snapshots, every bug report is "this site extracts wrong", and
// reproducing it means the maintainer needs the site to still be up, serving
// the same content, to the same Chromium build, on the same platform. Half of
// those conditions expire within a week.
//
// With snapshots, a user attaches one file and the maintainer reproduces the
// entire graph stage offline and deterministically. It is also what makes
// golden-file tests possible at all: extraction quality regressions are silent,
// and a diff against a stored artifact is the only cheap way to catch them.
//
// The snapshot deliberately stores the *capture*, not the artifact. Storing the
// artifact would only prove what the graph produced last time; storing the
// capture lets the whole graph stage be re-run against new code.
package snapshot

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/graph"
)

// FormatVersion is bumped when the snapshot layout changes incompatibly.
const FormatVersion = 1

// Snapshot is a recorded capture plus everything needed to interpret it.
type Snapshot struct {
	FormatVersion int       `json:"format_version"`
	RecordedAt    time.Time `json:"recorded_at"`

	RequestedURL string `json:"requested_url"`
	FinalURL     string `json:"final_url,omitempty"`
	Status       int64  `json:"status,omitempty"`

	// Trace is the complete set of inputs that determined the render. A
	// snapshot without it is not replayable, only inspectable.
	Trace any `json:"trace"`

	// Merged is the deduplicated capture: the input to the graph stage.
	Merged *capture.Merged `json:"merged"`
	// Scene and Libraries are the page-level probes.
	Scene     *capture.SceneIntrospection `json:"scene,omitempty"`
	Libraries []string                    `json:"libraries,omitempty"`

	// StaticHTML is the served document, retained so tier-0 extraction can be
	// replayed too and so the escalation decision can be re-scored.
	StaticHTML string `json:"static_html,omitempty"`

	Notes []string `json:"notes,omitempty"`

	// Redacted records that content was removed before writing. A snapshot from
	// an authenticated session must never be attachable to a public bug report
	// with the session's contents intact.
	Redacted bool   `json:"redacted,omitempty"`
	RedactionReason string `json:"redaction_reason,omitempty"`
}

// ErrPrivateSession is returned when a snapshot is requested for a session that
// was authenticated. Refusing is the correct default: a trace file is something
// a user attaches to a public issue, and a page from behind a login has no
// business in one.
var ErrPrivateSession = fmt.Errorf(
	"refusing to record a snapshot of an authenticated session: " +
		"the resulting file would contain content from behind a login and is " +
		"intended to be attached to bug reports. Re-run without --profile, or " +
		"pass --allow-private-snapshot if you have checked the contents yourself")

// WriteOptions controls recording.
type WriteOptions struct {
	// Private marks the session as authenticated.
	Private bool
	// AllowPrivate overrides the refusal, for a user who has decided the
	// contents are safe to share.
	AllowPrivate bool
	// IncludeHTML retains the served document. It is the largest part of a
	// snapshot and the most likely to carry something personal, so it is opt-in
	// for private sessions even when they are allowed.
	IncludeHTML bool
}

// Write records a snapshot to a gzipped JSON file.
func Write(path string, s *Snapshot, opt WriteOptions) error {
	if opt.Private && !opt.AllowPrivate {
		return ErrPrivateSession
	}
	if opt.Private {
		// Allowed, but still trimmed. The served HTML of an authenticated page
		// is where session tokens, personal details and draft content live.
		s.Redacted = true
		s.RedactionReason = "recorded from an authenticated session; served HTML omitted"
		if !opt.IncludeHTML {
			s.StaticHTML = ""
		}
	}
	s.FormatVersion = FormatVersion
	if s.RecordedAt.IsZero() {
		s.RecordedAt = time.Now().UTC()
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Snapshots are mostly repetitive text and compress to roughly a tenth of
	// their size, which is the difference between an attachable file and one
	// that has to be uploaded somewhere.
	zw := gzip.NewWriter(f)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// Read loads a snapshot.
func Read(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") || strings.HasSuffix(path, ".sieve") {
		zr, zerr := gzip.NewReader(f)
		if zerr != nil {
			return nil, fmt.Errorf("read %s: %w", path, zerr)
		}
		defer zr.Close()
		r = zr
	}

	var s Snapshot
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("%s was written by a newer sieve (format %d, this build understands %d)",
			path, s.FormatVersion, FormatVersion)
	}
	if s.Merged == nil {
		return nil, fmt.Errorf("%s contains no capture", path)
	}
	return &s, nil
}

// Replay rebuilds a graph from a snapshot, with no browser and no network.
//
// This is the function a maintainer runs against a user's attached file. It is
// also what golden-file tests run, so the test corpus exercises exactly the
// code path that a bug report exercises.
func Replay(s *Snapshot, in graph.Input) (*graph.Graph, error) {
	in.RequestedURL = s.RequestedURL
	in.FinalURL = s.FinalURL
	in.Merged = s.Merged
	if in.Notes == nil {
		in.Notes = s.Notes
	}
	if in.OriginalText == "" {
		in.OriginalText = s.StaticHTML
	}
	if in.OriginalBytes == 0 {
		in.OriginalBytes = int64(len(s.StaticHTML))
	}
	if in.Provenance.Trace == nil {
		in.Provenance.Trace = s.Trace
	}
	if in.Provenance.Libraries == nil {
		in.Provenance.Libraries = s.Libraries
	}
	return graph.Build(in)
}

// Ext is the conventional file extension.
const Ext = ".sieve"

// DefaultPath derives a snapshot filename from a URL.
func DefaultPath(dir, rawURL string) string {
	name := rawURL
	name = strings.TrimPrefix(name, "https://")
	name = strings.TrimPrefix(name, "http://")
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_':
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, "-")
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "snapshot"
	}
	return filepath.Join(dir, name+Ext)
}
