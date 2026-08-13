package snapshot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/snapshot"
	"github.com/qcoderx/sieve/internal/static"
)

const servedPage = `<!doctype html>
<html lang="en"><head><title>Halstead Instruments</title></head>
<body>
  <nav><a href="/">Home</a><a href="/about">About</a></nav>
  <main>
    <h1>Halstead Instruments</h1>
    <p>We restore and calibrate laboratory balances, and hold the only
       remaining stock of knife edges for the Oertling range.</p>
    <h2>Workshop</h2>
    <p>Calibration is traceable and certificates are issued the same week.
       A restoration takes eight to twelve weeks depending on what we find
       once the case is open.</p>
  </main>
</body></html>`

// TestTierZeroSnapshotRoundTrips covers the pages a snapshot was least able to
// describe: the ones answered without a browser.
//
// A tier-0 answer is the commonest answer sieve gives, and the result for that
// path carried neither the capture nor the served bytes -- so -snapshot wrote a
// file containing little more than a URL, and replay rejected it with
// "contains no capture", which reads like a corrupt file rather than one that
// was never given anything to hold. The effect was that the pages whose bugs
// are cheapest to reproduce were exactly the ones no one could attach to a
// report; every text-welding and reading-order defect fixed in this branch was
// a tier-0 defect.
func TestTierZeroSnapshotRoundTrips(t *testing.T) {
	res, err := static.Extract("https://example.com/", strings.NewReader(servedPage), len(servedPage))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	in := graph.Input{
		RequestedURL: "https://example.com/",
		FinalURL:     "https://example.com/",
		Merged:       res.Merged,
		Generator:    "sieve/test",
	}
	live, err := graph.Build(in)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(live.Blocks) == 0 {
		t.Fatal("fixture produced no blocks; the test would prove nothing")
	}

	// A snapshot as the tier-0 path records one: served HTML, no capture.
	path := filepath.Join(t.TempDir(), "tier0.sieve")
	if err := snapshot.Write(path, &snapshot.Snapshot{
		RequestedURL: "https://example.com/",
		FinalURL:     "https://example.com/",
		StaticHTML:   servedPage,
	}, snapshot.WriteOptions{IncludeHTML: true}); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := snapshot.Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rebuilt, err := snapshot.Replay(got, graph.Input{Generator: "sieve/test"})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if len(rebuilt.Blocks) != len(live.Blocks) {
		t.Fatalf("replay produced %d blocks, live produced %d",
			len(rebuilt.Blocks), len(live.Blocks))
	}
	for i := range live.Blocks {
		if live.Blocks[i].Text != rebuilt.Blocks[i].Text {
			t.Errorf("block %d differs:\n live=%q\n repl=%q",
				i, live.Blocks[i].Text, rebuilt.Blocks[i].Text)
		}
	}
	if live.ContentHash != rebuilt.ContentHash {
		t.Errorf("content hash differs: the graph stage is not a pure function "+
			"of the snapshot\n live=%s\n repl=%s", live.ContentHash, rebuilt.ContentHash)
	}
}

// TestSnapshotWithNothingInItIsRefused keeps the error honest: a file that
// carries neither a capture nor the served bytes cannot be replayed, and should
// say so rather than appearing to work.
func TestSnapshotWithNothingInItIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sieve")
	if err := snapshot.Write(path, &snapshot.Snapshot{
		RequestedURL: "https://example.com/",
	}, snapshot.WriteOptions{}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := snapshot.Read(path); err == nil {
		t.Error("a snapshot with no capture and no HTML was accepted")
	}
}
