package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/qcoderx/sieve/internal/bench"
	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/fetch"
	"github.com/qcoderx/sieve/internal/llm"
	"github.com/qcoderx/sieve/internal/safety"
)

func runBench(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		common    commonFlags
		questions string
		model     string
		grader    string
		out       string
		stability bool
		budget    int64
	)
	common.register(fs)
	fs.StringVar(&questions, "questions", "", "path to a question set (YAML or JSON)")
	fs.StringVar(&model, "model", "", "model to answer with")
	fs.StringVar(&grader, "grader-model", "", "model to grade with (defaults to the answering model)")
	fs.StringVar(&out, "report", "", "write the JSON report to this path")
	fs.BoolVar(&stability, "stability", false,
		"distill twice and report tier and content stability instead of running questions")
	fs.Int64Var(&budget, "budget", 2_000_000, "total token ceiling for the run")

	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: sieve bench <artifact-dir> --questions <file> [flags]
       sieve bench <url> --stability [flags]

Answers a question set twice -- once from the raw page, once from the artifact --
with the same model, the same prompt and the same budget, then grades both
against hand-written ground truth.

The numbers reported are measurements, not estimates: token counts come from the
API's own accounting.

Success criteria for v1:
  token reduction  >= 90%
  accuracy gain    >= 20 points
  coverage         >= 0.90
  fidelity         >= 0.98   (a distiller that invents content is worse than none)

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
	target := positional[0]

	if stability {
		return runStability(target, common, out, stdout, stderr)
	}

	if questions == "" {
		fmt.Fprintln(stderr, "sieve: --questions is required unless --stability is set")
		return 2
	}
	if !llm.HasCredentials("") {
		return fail(stderr, llm.ErrNoCredentials)
	}

	set, err := bench.LoadSet(questions)
	if err != nil {
		return fail(stderr, err)
	}
	g, err := emit.LoadGraph(target)
	if err != nil {
		return fail(stderr, fmt.Errorf("load artifact from %s: %w", target, err))
	}

	// The raw condition needs the page as an unaided agent would receive it, so
	// it is fetched fresh rather than reconstructed from the artifact.
	fmt.Fprintf(stderr, "fetching the raw page for the control condition…\n")
	raw, err := fetchRaw(set.URL, common)
	if err != nil {
		return fail(stderr, err)
	}

	opts := bench.DefaultOptions()
	opts.Budget = budget
	if model != "" {
		opts.Model = model
	}
	if grader != "" {
		opts.GraderModel = grader
	}
	if common.verbose {
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(stderr, "  "+f+"\n", a...) }
	}

	runner, err := bench.NewRunner(opts)
	if err != nil {
		return fail(stderr, err)
	}

	ctx, cancel := withTimeout(common.timeout * 4)
	defer cancel()

	fmt.Fprintf(stderr, "answering %d questions under both conditions with %s…\n",
		len(set.Questions), opts.Model)
	rep, err := runner.Run(ctx, bench.Input{Set: set, Artifact: g, RawHTML: raw})
	if err != nil {
		return fail(stderr, err)
	}

	printReport(stdout, rep)
	a, gr := runner.Usage()
	fmt.Fprintf(stderr, "\nAPI usage: %d answering calls, %d grading calls, %d tokens total\n",
		a.Calls, gr.Calls, a.Total()+gr.Total())

	if out != "" {
		if err := writeJSON(out, rep); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "report written to %s\n", out)
	}
	if !rep.Verdict.Passed {
		return 1
	}
	return 0
}

func runStability(target string, common commonFlags, out string, stdout, stderr io.Writer) int {
	opts := distill.DefaultOptions()
	opts.Guard = common.guard()
	opts.Limiter = common.limiter()
	opts.Memory = loadMemory(common.memoryPath)
	opts.Robots = safety.NewRobotsCache(nil)
	opts.Render.ChromePath = common.chrome
	if common.verbose {
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(stderr, "  "+f+"\n", a...) }
	}

	d := distill.New(opts)
	defer d.Close()

	ctx, cancel := withTimeout(common.timeout * 3)
	defer cancel()

	fmt.Fprintf(stderr, "distilling %s (run 1 of 2)…\n", target)
	a, err := d.Distill(ctx, target)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "distilling %s (run 2 of 2)…\n", target)
	b, err := d.Distill(ctx, target)
	if err != nil {
		return fail(stderr, err)
	}

	s := bench.MeasureStability(a.Graph, b.Graph)
	printStability(stdout, target, s)

	if out != "" {
		if err := writeJSON(out, s); err != nil {
			return fail(stderr, err)
		}
	}
	if !s.TierStable || s.BlockAgreement < 0.95 {
		return 1
	}
	return 0
}

func printReport(w io.Writer, r *bench.Report) {
	fmt.Fprintf(w, "\n%s\n", r.Target)
	fmt.Fprintf(w, "  model %s, grader %s, tier %s\n\n", r.Model, r.GraderModel, r.Tier)

	fmt.Fprintf(w, "  %-14s %12s %12s\n", "", "raw page", "artifact")
	fmt.Fprintf(w, "  %-14s %12.3f %12.3f\n", "accuracy",
		r.Metrics.Raw.MeanAccuracy, r.Metrics.Artifact.MeanAccuracy)
	fmt.Fprintf(w, "  %-14s %12.0f %12.0f\n", "input tokens",
		r.Metrics.Raw.MeanInputToks, r.Metrics.Artifact.MeanInputToks)
	fmt.Fprintf(w, "  %-14s %12.0f %12.0f\n", "latency ms",
		r.Metrics.Raw.MeanLatencyMS, r.Metrics.Artifact.MeanLatencyMS)

	bands := map[bench.Band]bool{}
	for b := range r.Metrics.Raw.ByBand {
		bands[b] = true
	}
	for b := range r.Metrics.Artifact.ByBand {
		bands[b] = true
	}
	var names []string
	for b := range bands {
		names = append(names, string(b))
	}
	sort.Strings(names)
	for _, n := range names {
		b := bench.Band(n)
		fmt.Fprintf(w, "  %-14s %12.3f %12.3f\n", "  "+n,
			r.Metrics.Raw.ByBand[b], r.Metrics.Artifact.ByBand[b])
	}

	fmt.Fprintf(w, "\n  coverage      %.3f   (ground-truth facts present in the artifact)\n", r.Coverage)
	fmt.Fprintf(w, "  fidelity      %.3f   (artifact statements verifiable in the source)\n", r.Fidelity)
	for _, n := range r.FidelityNotes {
		fmt.Fprintf(w, "                %s\n", n)
	}

	fmt.Fprintf(w, "\n  token reduction %.1f%%   accuracy gain %+.1f points\n",
		r.Verdict.TokenReduction*100, r.Verdict.AccuracyGain*100)
	fmt.Fprintf(w, "\n  %s\n\n", r.Verdict.Summary)
}

func printStability(w io.Writer, target string, s bench.Stability) {
	fmt.Fprintf(w, "\nstability: %s\n\n", target)
	fmt.Fprintf(w, "  tier            %s / %s  %s\n", s.TierA, s.TierB, okMark(s.TierStable))
	fmt.Fprintf(w, "  content hash    %s\n", okMark(s.HashStable))
	fmt.Fprintf(w, "  block agreement %.3f  (%d and %d blocks)\n",
		s.BlockAgreement, s.BlocksA, s.BlocksB)
	if len(s.OnlyInA) > 0 || len(s.OnlyInB) > 0 {
		fmt.Fprintf(w, "\n  blocks that differed between runs:\n")
		for _, t := range s.OnlyInA {
			fmt.Fprintf(w, "    only in run 1: %s\n", t)
		}
		for _, t := range s.OnlyInB {
			fmt.Fprintf(w, "    only in run 2: %s\n", t)
		}
		fmt.Fprintf(w, "\n  A genuinely personalised page will differ here, and that is the\n"+
			"  honest result rather than a defect.\n")
	}
	fmt.Fprintln(w)
}

func okMark(ok bool) string {
	if ok {
		return "stable"
	}
	return "UNSTABLE"
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// fetchRaw retrieves the page as an unaided agent would receive it.
func fetchRaw(target string, common commonFlags) (string, error) {
	ctx, cancel := withTimeout(30 * time.Second)
	defer cancel()

	fo := fetch.DefaultOptions()
	fo.Guard = common.guard()
	resp, err := fetch.New(fo).Get(ctx, target, nil)
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}
