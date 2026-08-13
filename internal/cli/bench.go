package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
		rawCap    int
		regrade   int

		coverageOnly bool
		tokensOnly   bool
	)
	common.register(fs)
	fs.StringVar(&questions, "questions", "", "path to a question set (YAML or JSON)")
	fs.StringVar(&model, "model", "", "model to answer with")
	fs.StringVar(&grader, "grader-model", "", "model to grade with (defaults to the answering model)")
	fs.StringVar(&out, "report", "", "write the JSON report to this path")
	fs.BoolVar(&stability, "stability", false,
		"distill twice and report tier and content stability instead of running questions")
	fs.Int64Var(&budget, "budget", 2_000_000, "total token ceiling for the run")
	fs.IntVar(&rawCap, "raw-context", 100_000,
		"how much of the raw page the control is given, in tokens.\n"+
			"Models what an unaided agent actually receives: a larger page is\n"+
			"truncated and the report says by how much. 0 sends all of it, which\n"+
			"simply fails on anything large.")
	fs.IntVar(&regrade, "regrade", 0,
		"re-grade this many answers to measure how far the grader agrees with\n"+
			"itself. It is the error bar on every accuracy figure; 12 is useful.")
	fs.BoolVar(&tokensOnly, "tokens", false,
		"distill a URL and report what an agent receives for one page read,\n"+
			"against an unaided fetch of the same page. No model is called and no\n"+
			"credentials are needed, so anyone can reproduce it in the time it\n"+
			"takes to read the claim.")
	fs.BoolVar(&coverageOnly, "coverage-only", false,
		"check which ground-truth facts are present in the artifact and stop.\n"+
			"No model is called and no credentials are needed. This is how to\n"+
			"check a question set you have just written: a fact whose wording\n"+
			"appears nowhere is usually a mistake in the set rather than a loss\n"+
			"in the extraction, and finding that out should not cost a run.")

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
	if tokensOnly {
		return runTokens(target, common, out, stdout, stderr)
	}

	if questions == "" {
		fmt.Fprintln(stderr, "sieve: --questions is required unless --stability is set")
		return 2
	}
	// Credentials are only needed once a model is going to be called.
	if !coverageOnly && !llm.HasCredentials("") {
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

	if coverageOnly {
		printCoverage(stdout, set, bench.CheckCoverage(set, g))
		return 0
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
	opts.RawContextTokens = rawCap
	opts.GraderRepeats = regrade
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

	// The benchmark's own deadline, not the page-reading one.
	//
	// This used to be four times -timeout, which is the budget for reading a
	// single page: forty seconds by default, to cover forty model calls that
	// each take a second or more and may be asked by the provider to wait out
	// a rate limit. The run died of its own deadline and reported the survivors
	// as if they were the whole measurement.
	//
	// A minute per question, floored at ten, is generous enough that only a
	// genuinely stuck run hits it, and bounded enough that a stuck run still
	// ends.
	runBudget := time.Duration(len(set.Questions)) * time.Minute
	if runBudget < 10*time.Minute {
		runBudget = 10 * time.Minute
	}
	ctx, cancel := withTimeout(runBudget)
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
	// Answered comes before accuracy, because an accuracy computed over three
	// of twenty questions is a different claim from one computed over twenty,
	// and the reader should see which before reading the number.
	if r.Metrics.Raw.Ungraded > 0 || r.Metrics.Artifact.Ungraded > 0 {
		fmt.Fprintf(w, "  %-14s %12s %12s   (accuracy is the mean over these)\n", "graded",
			fmt.Sprintf("%d/%d", r.Metrics.Raw.Scored, r.Metrics.Raw.Answered),
			fmt.Sprintf("%d/%d", r.Metrics.Artifact.Scored, r.Metrics.Artifact.Answered))
	}
	fmt.Fprintf(w, "  %-14s %12s %12s\n", "answered",
		fmt.Sprintf("%d/%d", r.Metrics.Raw.Answered, r.Metrics.Raw.Questions),
		fmt.Sprintf("%d/%d", r.Metrics.Artifact.Answered, r.Metrics.Artifact.Questions))
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

	if r.RawTruncated {
		fmt.Fprintf(w, "\n  raw page      %d tokens, of which %d fitted (%.0f%% withheld)\n",
			r.RawTokens, r.RawTokensSent,
			100*(1-float64(r.RawTokensSent)/float64(r.RawTokens)))
		fmt.Fprintf(w, "                the control got the top of the page and no more, which\n")
		fmt.Fprintf(w, "                is what an unaided agent gets on a page this size\n")
	}
	if r.GraderRegraded > 0 {
		fmt.Fprintf(w, "\n  grader agreed with itself on %.0f%% of %d re-graded answers\n",
			r.GraderAgreement*100, r.GraderRegraded)
		if r.GraderAgreement < 0.9 {
			fmt.Fprintf(w, "                treat differences smaller than that spread as noise\n")
		}
	}
	fmt.Fprintf(w, "\n  compared      %d question(s) answered under both conditions\n",
		r.Verdict.ComparedQuestions)
	fmt.Fprintf(w, "  token reduction %.1f%%   accuracy gain %+.1f points\n",
		r.Verdict.TokenReduction*100, r.Verdict.AccuracyGain*100)
	// The reasons calls failed, before the verdict rather than after it.
	//
	// A run that reports forty errors and no cause leaves the reader to open
	// the JSON, which they can only do if they thought to pass -report. The
	// provider's own message is nearly always the whole answer -- a model name
	// that does not exist, a key without access, a rate limit -- so it belongs
	// on screen where the failure is announced.
	if len(r.CallFailures) > 0 {
		fmt.Fprintf(w, "\n  calls failed:\n")
		for i, f := range r.CallFailures {
			if i == 3 {
				fmt.Fprintf(w, "    ... and %d further distinct reason(s), all in the report\n",
					len(r.CallFailures)-3)
				break
			}
			fmt.Fprintf(w, "    %3d x  %s\n", f.Count, oneLine(f.Reason, 150))
		}
	}
	fmt.Fprintf(w, "\n  %s\n", r.Verdict.Summary)
	for _, n := range r.Verdict.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
	fmt.Fprintln(w)
}

// oneLine flattens a provider message so it cannot break the report's layout.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max-3] + "..."
	}
	return s
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

// printCoverage reports which ground-truth facts the artifact carries.
//
// The missing ones are the point. A bare score says something is wrong without
// saying what, and the two causes look identical from the number alone: the
// extraction lost content, or the fact was written in words the page never
// used. Printing the words that appear nowhere lets the author tell those apart
// in a second, and the wording is often the answer.
func printCoverage(w io.Writer, set *bench.Set, res bench.CoverageResult) {
	fmt.Fprintf(w, "\n%s\n", set.URL)
	fmt.Fprintf(w, "  coverage %.3f  (%d of %d ground-truth facts present)\n\n",
		res.Coverage, res.Found, res.Total)

	missing := 0
	for _, f := range res.Facts {
		if f.Present {
			continue
		}
		missing++
		fmt.Fprintf(w, "  missing  %-5s %q\n", f.QuestionID, f.Fact)
		if len(f.Missing) > 0 {
			fmt.Fprintf(w, "           words absent from the artifact: %s\n",
				strings.Join(f.Missing, ", "))
		}
	}
	if missing == 0 {
		fmt.Fprintf(w, "  every fact in the set is present in the artifact\n")
	} else {
		fmt.Fprintf(w, "\n  %d fact(s) missing. Before treating these as extraction failures,\n", missing)
		fmt.Fprintf(w, "  check them against the page's own source: a fact the page never\n")
		fmt.Fprintf(w, "  states, or states in other words, is a fault in the question set.\n")
	}
	fmt.Fprintln(w)
}
