package cli

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/snapshot"
)

func runReplay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		out     string
		showAll bool
	)
	fs.StringVar(&out, "out", "", "write the rebuilt artifact to this directory")
	fs.BoolVar(&showAll, "blocks", false, "print every block")

	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: sieve replay <snapshot.sieve> [flags]

Rebuilds an artifact from a recorded capture, with no browser and no network.

This is what makes a bug report actionable. A user attaches one file and the
whole graph stage -- reassembly, classification, heading inference, ordering --
runs against it deterministically, on any machine, long after the site has
changed.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, perr := parseArgs(fs, args)
	if perr != nil {
		return 2
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}

	snap, err := snapshot.Read(positional[0])
	if err != nil {
		return fail(stderr, err)
	}

	// The timestamp is fixed so that replaying the same snapshot twice produces
	// byte-identical output. A replay whose result moved would be useless for
	// diffing against a golden file.
	g, err := snapshot.Replay(snap, graph.Input{
		Now:       time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		Generator: "sieve/" + render.Version + " (replay)",
	})
	if err != nil {
		return fail(stderr, err)
	}

	fmt.Fprintf(stdout, "replayed %s\n", snap.RequestedURL)
	fmt.Fprintf(stdout, "  recorded   %s\n", snap.RecordedAt.Format(time.RFC3339))
	if snap.Redacted {
		fmt.Fprintf(stdout, "  redacted   %s\n", snap.RedactionReason)
	}
	if len(snap.Libraries) > 0 {
		fmt.Fprintf(stdout, "  libraries  %v\n", snap.Libraries)
	}
	fmt.Fprintf(stdout, "  capture    %d nodes, %d latent, %d checkpoints\n",
		len(snap.Merged.Nodes), len(snap.Merged.Latent), snap.Merged.Checkpoints)
	fmt.Fprintf(stdout, "  graph      %d blocks, %d sections, hash %s\n",
		len(g.Blocks), len(g.Sections), g.ContentHash)
	fmt.Fprintf(stdout, "  audit      retention %.1f%%, order %s (%s), headings %s\n",
		g.Audit.GraphRetention*100, g.Audit.OrderConfidence, g.Audit.OrderBasis,
		g.Audit.HeadingConfidence)

	if snap.Trace != nil {
		fmt.Fprintf(stdout, "\n  trace\n")
		printTrace(stdout, snap.Trace)
	}

	if showAll {
		fmt.Fprintln(stdout)
		for _, b := range g.Blocks {
			lvl := ""
			if b.Level > 0 {
				lvl = fmt.Sprintf("h%d ", b.Level)
			}
			fmt.Fprintf(stdout, "  %s [%s/%s] %s%s\n", b.ID, b.Type, b.Region, lvl, truncate(b.Text, 100))
		}
	}

	if out != "" {
		art, err := emit.Build(g)
		if err != nil {
			return fail(stderr, err)
		}
		if err := art.Write(out); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "\nwrote %s\n", out)
	}
	return 0
}

func printTrace(w io.Writer, trace any) {
	m, ok := trace.(map[string]any)
	if !ok {
		fmt.Fprintf(w, "    %v\n", trace)
		return
	}
	for _, k := range sortedKeys(m) {
		v := m[k]
		if s, isSlice := v.([]any); isSlice && len(s) > 6 {
			fmt.Fprintf(w, "    %-22s %d entries\n", k, len(s))
			continue
		}
		fmt.Fprintf(w, "    %-22s %v\n", k, v)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
