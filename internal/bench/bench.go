// Package bench measures whether the distillation is actually worth doing.
//
// This is the core deliverable, not an afterthought. The claim is that an agent
// given an artifact answers better, faster and far more cheaply than the same
// agent given the raw page, and a claim like that is worth exactly as much as
// the harness behind it.
//
// # What is measured, and what each number is allowed to mean
//
//	Token cost      Input tokens the API actually charged. A measurement, not
//	                an estimate: both conditions report usage from the same API.
//	Time to answer  Wall clock for the answering call.
//	Accuracy        Graded against hand-written ground truth, 0 to 1.
//	Coverage        Share of ground-truth facts present in the artifact at all.
//	                This is the *real* coverage number -- the one measured where
//	                the right answer is known. The per-artifact self-audit
//	                deliberately calls its figure "graph retention" instead,
//	                because that one compares output against what the capture
//	                observed and would cheerfully report 100% of a page it only
//	                half saw.
//	Fidelity        Share of artifact statements verifiable in the source. A
//	                distiller that invents content is worse than no distiller,
//	                so this one gates the release.
//	Stability       Distill twice, compare. Reported separately for the tier
//	                decision and for the content, because a tool that wavers
//	                between tiers and one that extracts inconsistently are
//	                different failures with different causes.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/llm"
)

// Band groups questions by what they test.
type Band string

const (
	// BandFactual: what is the founding year, what materials are listed.
	BandFactual Band = "factual"
	// BandStructural: what sections exist, what is the navigation hierarchy.
	BandStructural Band = "structural"
	// BandActionable: how does a visitor make an enquiry, what fields are required.
	BandActionable Band = "actionable"
)

// Question is one item in a question set.
type Question struct {
	ID   string `yaml:"id" json:"id"`
	Band Band   `yaml:"band" json:"band"`
	Ask  string `yaml:"ask" json:"ask"`
	// Expect is the ground-truth answer, written by hand against the real page.
	Expect string `yaml:"expect" json:"expect"`
	// Facts are the atomic claims a correct answer must contain. They are what
	// coverage is measured against, and they are why coverage means something
	// here and not in the self-audit.
	Facts []string `yaml:"facts" json:"facts"`
}

// Set is a question set for one target.
type Set struct {
	URL       string     `yaml:"url" json:"url"`
	Name      string     `yaml:"name" json:"name"`
	Questions []Question `yaml:"questions" json:"questions"`
}

// LoadSet reads a question set from YAML or JSON.
func LoadSet(path string) (*Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Set
	if strings.HasSuffix(path, ".json") {
		err = json.Unmarshal(b, &s)
	} else {
		err = yaml.Unmarshal(b, &s)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(s.Questions) == 0 {
		return nil, fmt.Errorf("%s contains no questions", path)
	}
	for i := range s.Questions {
		if s.Questions[i].ID == "" {
			s.Questions[i].ID = fmt.Sprintf("q%02d", i+1)
		}
	}
	return &s, nil
}

// Condition names one side of the comparison.
type Condition string

const (
	// ConditionRaw gives the agent the served HTML, as an unaided agent would
	// have to read it.
	ConditionRaw Condition = "raw"
	// ConditionArtifact gives the agent the distilled Markdown.
	ConditionArtifact Condition = "artifact"
)

// Answer is one question answered under one condition.
type Answer struct {
	QuestionID string    `json:"question_id"`
	Condition  Condition `json:"condition"`
	Text       string    `json:"text"`
	Usage      llm.Usage `json:"usage"`
	LatencyMS  int64     `json:"latency_ms"`
	Refused    bool      `json:"refused,omitempty"`
	Error      string    `json:"error,omitempty"`

	// Accuracy and FactsFound are filled by the grader.
	Accuracy    float64  `json:"accuracy"`
	FactsFound  []string `json:"facts_found,omitempty"`
	FactsMissed []string `json:"facts_missed,omitempty"`
	GraderNote  string   `json:"grader_note,omitempty"`

	// Graded records that a grade was actually reached.
	//
	// An answer that was produced but never graded -- the grader rate limited,
	// out of quota, or returning unparseable JSON -- has an Accuracy of zero
	// that means "not measured", not "wrong". The distinction is the same one
	// Error draws for the answering call, and it was missing here: a run that
	// answered every question and graded none reported an accuracy of 0.000
	// for both conditions, which reads as total failure of the extraction when
	// nothing had been measured at all.
	Graded bool `json:"graded"`
}

// CallFailure is one reason calls failed, and how many times it did.
type CallFailure struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// Report is a complete benchmark run.
type Report struct {
	Target      string    `json:"target"`
	GeneratedAt time.Time `json:"generated_at"`
	Model       string    `json:"model"`
	GraderModel string    `json:"grader_model"`
	ContentHash string    `json:"content_hash"`
	Tier        string    `json:"tier"`

	// RawTokens is the size of the page an unaided agent would have been
	// handed, and RawTokensSent is how much of it actually fitted.
	//
	// These differ on exactly the pages sieve exists for. A context ceiling is
	// not a limitation of this benchmark, it is the condition the control is
	// under in real use -- an agent given a four-hundred-thousand-token page
	// does not read four hundred thousand tokens of it either. Recording both
	// is what keeps that honest rather than hidden.
	RawTokens     int  `json:"raw_tokens"`
	RawTokensSent int  `json:"raw_tokens_sent"`
	RawTruncated  bool `json:"raw_truncated"`

	// RawVisibleChars is how much readable text the served page carried, with
	// markup and scripts removed. It decides which criteria can be judged at
	// all: a page that serves nothing can neither verify the artifact nor be
	// reduced in size.
	RawVisibleChars int `json:"raw_visible_chars"`

	Answers []Answer `json:"answers"`

	// CallFailures groups the distinct reasons calls failed, commonest first.
	//
	// A failure count on its own is a dead end. The reason is always in the
	// report's per-answer records, but a reader watching the terminal sees only
	// a number, and the difference between a wrong model name, an expired key
	// and a rate limit is the difference between a five-second fix and an
	// afternoon. The provider already says which it is; this is only a matter
	// of not throwing it away.
	CallFailures []CallFailure `json:"call_failures,omitempty"`

	Metrics struct {
		Raw      ConditionMetrics `json:"raw"`
		Artifact ConditionMetrics `json:"artifact"`
	} `json:"metrics"`

	// Coverage is measured against ground-truth facts, not against what the
	// capture happened to observe.
	Coverage float64 `json:"coverage"`
	// Fidelity is the share of sampled artifact statements verifiable in the
	// source. It gates the release: a distiller that invents content is worse
	// than no distiller.
	Fidelity float64 `json:"fidelity"`
	// FidelityMeasured distinguishes a fidelity of zero from a check that could
	// not run. They are opposite conclusions -- one says the artifact invented
	// everything, the other that nothing could be verified -- and collapsing
	// them into a single number condemned sieve for extracting content which
	// was correctly absent from the served HTML.
	FidelityMeasured bool     `json:"fidelity_measured"`
	FidelityNotes    []string `json:"fidelity_notes,omitempty"`

	// GraderAgreement is how often the grader gave the same verdict when asked
	// twice about the same answer. It is the error bar on every accuracy figure
	// above: a grader that disagrees with itself a fifth of the time cannot
	// support a claim of a five-point difference, and without measuring it
	// there is no way to know which claims are safe.
	GraderAgreement float64 `json:"grader_agreement,omitempty"`
	GraderRegraded  int     `json:"grader_regraded,omitempty"`

	// Stability is reported only when a stability run was requested.
	Stability *Stability `json:"stability,omitempty"`

	// Verdict states plainly whether the run met the success criteria.
	Verdict Verdict `json:"verdict"`
}

// ConditionMetrics aggregates one side of the comparison.
type ConditionMetrics struct {
	Questions int `json:"questions"`
	// Answered is how many questions actually produced an answer. It differs
	// from Questions whenever a call failed, and the accuracy below is the mean
	// over these rather than over all of them.
	Answered int `json:"answered"`
	// Scored is how many of those answers were graded, and Ungraded how many
	// were not. Accuracy is the mean over Scored: an answer the grader never
	// reached is excluded rather than counted as wrong.
	Scored         int              `json:"scored"`
	Ungraded       int              `json:"ungraded"`
	MeanAccuracy   float64          `json:"mean_accuracy"`
	MeanInputToks  float64          `json:"mean_input_tokens"`
	TotalInputToks int64            `json:"total_input_tokens"`
	MeanLatencyMS  float64          `json:"mean_latency_ms"`
	Refusals       int              `json:"refusals"`
	Errors         int              `json:"errors"`
	ByBand         map[Band]float64 `json:"accuracy_by_band"`
}

// Stability is what two runs of the same URL produced.
type Stability struct {
	// TierStable reports whether both runs chose the same escalation tier.
	// It is separate from content stability on purpose: a tool that wavers
	// between tiers and one that extracts inconsistently are different bugs.
	TierStable bool   `json:"tier_stable"`
	TierA      string `json:"tier_a"`
	TierB      string `json:"tier_b"`

	// HashStable reports whether the semantic content hash matched.
	HashStable bool `json:"hash_stable"`
	// BlockAgreement is the share of blocks present in both runs with identical
	// text. A page that is genuinely personalised will score low here, and
	// correctly so.
	BlockAgreement float64  `json:"block_agreement"`
	BlocksA        int      `json:"blocks_a"`
	BlocksB        int      `json:"blocks_b"`
	OnlyInA        []string `json:"only_in_a,omitempty"`
	OnlyInB        []string `json:"only_in_b,omitempty"`
}

// Verdict states whether the run met the criteria for a release.
type Verdict struct {
	TokenReduction float64 `json:"token_reduction"`
	// AccuracyGain is the mean difference over questions answered under both
	// conditions, not the difference of the two means. See judge.
	AccuracyGain float64 `json:"accuracy_gain"`
	// ComparedQuestions is how many questions that pairing covered.
	ComparedQuestions int  `json:"compared_questions"`
	MetTokenTarget    bool `json:"met_token_target"`
	MetAccuracyTarget bool `json:"met_accuracy_target"`
	MetCoverageTarget bool `json:"met_coverage_target"`
	MetFidelityTarget bool `json:"met_fidelity_target"`
	// TokenReductionApplies is false on a page that served no readable text,
	// where there was nothing to reduce. MetTokenTarget is then true by
	// vacancy rather than by achievement, and this is how to tell them apart.
	TokenReductionApplies bool `json:"token_reduction_applies"`

	Passed  bool   `json:"passed"`
	Summary string `json:"summary"`
	// Notes record criteria that did not apply, so a pass is never mistaken
	// for having cleared a bar that was never raised.
	Notes []string `json:"notes,omitempty"`
}

// Targets are the success criteria for v1.
const (
	TargetTokenReduction = 0.90
	TargetAccuracyGain   = 0.20
	TargetCoverage       = 0.90
	// A distiller that invents content is worse than no distiller, so this is
	// the highest bar of the four and the one that gates the release.
	TargetFidelity = 0.98
)

// Options configures a run.
type Options struct {
	Model       string
	GraderModel string
	APIKey      string
	// Budget caps total tokens across the whole run.
	Budget int64
	// Concurrency bounds simultaneous API calls.
	Concurrency int
	// BaseURL points the run at an OpenAI-compatible provider. Empty means
	// Anthropic, or whatever LLM_BASE_URL names.
	BaseURL string
	// RawContextTokens caps how much of the raw page the control is given.
	//
	// Without a cap the control simply errors on any large page, so the
	// benchmark could measure everything except the case the tool exists for:
	// pear.no serves around four hundred and seventy thousand tokens and no
	// model accepts it. Truncating models what an unaided agent actually gets
	// -- the top of the document and no more -- and the report says how much
	// was left behind, so nobody mistakes the handicap for a result.
	RawContextTokens int
	// GraderRepeats re-grades a sample of answers to measure how much the
	// grader agrees with itself. Zero disables it.
	GraderRepeats int
	Logf          func(format string, args ...any)
}

// DefaultOptions returns usable settings.
//
// The model is left empty rather than pinned to the Anthropic default, because
// naming it here would override whatever provider the user configured. It used
// to be pinned, and the effect was that pointing sieve at another provider sent
// that provider a model name it had never heard of: a request to Groq asking
// for claude-opus-5, and a 404 that read like a broken build rather than a
// misconfiguration. Resolution belongs in one place, and that place is the
// client, which knows which provider it is talking to.
func DefaultOptions() Options {
	return Options{
		// The grader defaults to the answering model. Using a weaker one would
		// make the grade the weakest link in the measurement.
		Budget:      2_000_000,
		Concurrency: 4,
	}
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// computeMetrics aggregates the answers for one condition.
func computeMetrics(answers []Answer, questions map[string]Question, cond Condition) ConditionMetrics {
	m := ConditionMetrics{ByBand: map[Band]float64{}}
	bandSum := map[Band]float64{}
	bandCount := map[Band]int{}

	var accSum, latSum float64
	for _, a := range answers {
		if a.Condition != cond {
			continue
		}
		m.Questions++
		if a.Refused {
			m.Refusals++
		}
		if a.Error != "" {
			m.Errors++
			// A call that never completed is not a wrong answer.
			//
			// Counting it as one puts a zero in the numerator and a one in the
			// denominator, so a condition that failed outright scores 0.000 --
			// indistinguishable from a condition that answered every question
			// incorrectly. That is the wrong way round for this benchmark in
			// particular: the raw page is the control, it is the larger and
			// more failure-prone request of the two, and its collapse would be
			// reported as an accuracy gain for the artifact. A measurement that
			// flatters the thing it measures whenever the control breaks is
			// worse than no measurement.
			continue
		}
		m.Answered++
		latSum += float64(a.LatencyMS)
		m.TotalInputToks += a.Usage.InputTokens + a.Usage.CacheReadTokens

		// An answer that was never graded is not a wrong answer either.
		//
		// The same argument as above, one stage later in the pipeline. The
		// answering call succeeded, so Error is empty and the answer counts
		// towards cost and latency, which is right -- it really was produced.
		// But its Accuracy is zero only because no grade was ever reached, and
		// averaging that in reports the extraction as having failed when the
		// instrument was what failed.
		if !a.Graded {
			m.Ungraded++
			continue
		}
		m.Scored++
		accSum += a.Accuracy
		q := questions[a.QuestionID]
		bandSum[q.Band] += a.Accuracy
		bandCount[q.Band]++
	}
	if m.Scored > 0 {
		m.MeanAccuracy = round3(accSum / float64(m.Scored))
	}
	if m.Answered > 0 {
		m.MeanLatencyMS = round1(latSum / float64(m.Answered))
		m.MeanInputToks = round1(float64(m.TotalInputToks) / float64(m.Answered))
	}
	for b, sum := range bandSum {
		m.ByBand[b] = round3(sum / float64(bandCount[b]))
	}
	return m
}

// judge fills in the verdict.
func (r *Report) judge() {
	v := Verdict{}
	raw, art := r.Metrics.Raw, r.Metrics.Artifact

	// A comparison needs both sides to have happened.
	//
	// If a condition answered nothing -- rate limited, context too large, the
	// provider down -- there is no result to report, only a missing one. Left
	// to the arithmetic below, a collapsed control reads as an accuracy of
	// zero and hands the artifact a large apparent gain, which is the one
	// direction this benchmark must never fail in. Say it could not be
	// measured instead.
	if raw.Scored == 0 || art.Scored == 0 {
		// Distinguish nothing answered from nothing graded: they point at
		// different broken things, and the remedy differs.
		noun := "produced no answers"
		if raw.Answered > 0 && art.Answered > 0 {
			noun = "had no answer graded"
		}
		side := "the raw page " + noun
		if art.Scored == 0 {
			side = "the artifact " + noun
			if raw.Scored == 0 {
				side = "neither condition " + noun
				if noun == "produced no answers" {
					side = "neither condition produced any answers"
				}
			}
		}
		v.Summary = fmt.Sprintf(
			"could not be measured: %s (%d error(s) of %d question(s)); "+
				"no comparison is possible and none is reported",
			side, raw.Errors+art.Errors, raw.Questions)
		if why := r.dominantFailure(); why != "" {
			v.Summary += "; " + why
		}
		r.Verdict = v
		return
	}

	if raw.MeanInputToks > 0 {
		v.TokenReduction = round3(1 - art.MeanInputToks/raw.MeanInputToks)
	}

	// The gain is measured over the questions both conditions answered.
	//
	// Comparing the two headline means directly is only valid when both sides
	// answered the same questions, and they do not have to: a call can fail on
	// one side and succeed on the other, and the raw side fails more often
	// because its request is the larger one. On the fixture the raw condition
	// answered ten of twenty and the artifact all twenty, so the two means
	// described different question sets and their difference described nothing.
	//
	// Pairing them is the whole of the fix. Where every question succeeded on
	// both sides this is identical to subtracting the means.
	rawByID := map[string]float64{}
	for _, a := range r.Answers {
		if a.Condition == ConditionRaw && a.Graded {
			rawByID[a.QuestionID] = a.Accuracy
		}
	}
	var pairRaw, pairArt float64
	for _, a := range r.Answers {
		if a.Condition != ConditionArtifact || !a.Graded {
			continue
		}
		ra, ok := rawByID[a.QuestionID]
		if !ok {
			continue
		}
		v.ComparedQuestions++
		pairRaw += ra
		pairArt += a.Accuracy
	}
	if v.ComparedQuestions > 0 {
		v.AccuracyGain = round3((pairArt - pairRaw) / float64(v.ComparedQuestions))
	}

	// Token reduction only means something when there was something to reduce.
	//
	// On a page that serves an empty body -- the case this project exists for --
	// the control's prompt is a shell of a few hundred tokens and the artifact
	// carries the content the page actually renders. The artifact is then
	// legitimately larger, and scoring that as a failed criterion marks sieve
	// down precisely for having done its job. It is the same situation as
	// fidelity on such a page: not a failure, a criterion with no subject.
	//
	// The threshold is the one fidelity already uses, because it is the same
	// question being asked -- whether the served page carries readable text.
	v.TokenReductionApplies = r.RawVisibleChars >= minFidelitySource
	v.MetTokenTarget = !v.TokenReductionApplies || v.TokenReduction >= TargetTokenReduction
	v.MetAccuracyTarget = v.AccuracyGain >= TargetAccuracyGain
	v.MetCoverageTarget = r.Coverage >= TargetCoverage
	v.MetFidelityTarget = r.FidelityMeasured && r.Fidelity >= TargetFidelity
	v.Passed = v.MetTokenTarget && v.MetAccuracyTarget && v.MetCoverageTarget && v.MetFidelityTarget

	var missed []string
	if !v.MetTokenTarget {
		missed = append(missed, fmt.Sprintf("token reduction %.1f%% is below the %.0f%% target",
			v.TokenReduction*100, TargetTokenReduction*100))
	}
	if !v.TokenReductionApplies {
		v.Notes = append(v.Notes, fmt.Sprintf(
			"token reduction does not apply: the served page carries only %d characters of "+
				"readable text, so there was nothing to reduce and the artifact is larger by "+
				"exactly the content the page renders some other way",
			r.RawVisibleChars))
	}
	if !v.MetAccuracyTarget {
		missed = append(missed, fmt.Sprintf("accuracy gain %.1f points is below the %.0f-point target",
			v.AccuracyGain*100, TargetAccuracyGain*100))
	}
	if !v.MetCoverageTarget {
		missed = append(missed, fmt.Sprintf("coverage %.2f is below the %.2f target",
			r.Coverage, TargetCoverage))
	}
	if !v.MetFidelityTarget {
		if !r.FidelityMeasured {
			missed = append(missed, "fidelity was not measured on this page, so the "+
				"criterion that catches invention is unmet rather than failed")
		} else {
			missed = append(missed, fmt.Sprintf("fidelity %.3f is below the %.2f target — the artifact contains statements not verifiable in the source, which is the one failure that matters most",
				r.Fidelity, TargetFidelity))
		}
	}
	if v.Passed {
		v.Summary = fmt.Sprintf("passed: %.1f%% fewer tokens, %.1f points more accurate, coverage %.2f, fidelity %.3f",
			v.TokenReduction*100, v.AccuracyGain*100, r.Coverage, r.Fidelity)
	} else {
		v.Summary = "did not pass: " + strings.Join(missed, "; ")
	}
	// Partial data is still worth reporting, but never silently: a run where
	// some calls failed is a weaker claim than one where none did, and the
	// reader has to be told which they are holding.
	if n := raw.Errors + art.Errors; n > 0 {
		v.Summary += fmt.Sprintf(
			" — measured over the questions that completed; %d call(s) failed "+
				"(raw %d of %d, artifact %d of %d) and were excluded rather than "+
				"counted as wrong answers",
			n, raw.Errors, raw.Questions, art.Errors, art.Questions)
		if why := r.dominantFailure(); why != "" {
			v.Summary += "; " + why
		}
	}
	r.Verdict = v
}

// failureVariable matches the parts of a provider message that change between
// two occurrences of the same failure: retry delays, token tallies, request IDs.
var failureVariable = regexp.MustCompile(`\d[\d.,]*`)

// groupKey collapses a message to what is stable about it.
//
// A rate limit says how long to wait and how many tokens were used, and both
// differ every time. Grouping on the raw text turned eleven occurrences of one
// problem into "11 distinct failures", which reads as eleven problems and
// buries the single thing the reader needs to know. The numbers are worth
// keeping in the message shown -- just not in the identity of the group.
func groupKey(msg string) string {
	return failureVariable.ReplaceAllString(msg, "N")
}

// collectFailures groups the per-answer errors by cause, commonest first.
func (r *Report) collectFailures() {
	counts := map[string]int{}
	example := map[string]string{}
	note := func(msg string) {
		k := groupKey(msg)
		counts[k]++
		if _, seen := example[k]; !seen {
			example[k] = msg
		}
	}
	for _, a := range r.Answers {
		if a.Error != "" {
			note(a.Error)
			continue
		}
		// A grading failure is a failure of the measurement, and belongs in the
		// same list. It is easy to miss otherwise: the answers all arrived, so
		// nothing looks wrong until the accuracy column is inexplicably empty.
		if !a.Graded && strings.HasPrefix(a.GraderNote, "grading failed: ") {
			note(a.GraderNote)
		}
	}
	r.CallFailures = nil
	for k, n := range counts {
		r.CallFailures = append(r.CallFailures, CallFailure{Reason: example[k], Count: n})
	}
	sort.Slice(r.CallFailures, func(i, j int) bool {
		if r.CallFailures[i].Count != r.CallFailures[j].Count {
			return r.CallFailures[i].Count > r.CallFailures[j].Count
		}
		return r.CallFailures[i].Reason < r.CallFailures[j].Reason
	})
}

// dominantFailure names the commonest reason calls failed, for the summary.
//
// When every call failed for the same reason -- and they usually do, because
// the causes are configuration rather than chance -- that one line is the whole
// diagnosis. Truncated, because a provider is at liberty to return an essay.
func (r *Report) dominantFailure() string {
	if len(r.CallFailures) == 0 {
		return ""
	}
	top := r.CallFailures[0]
	reason := strings.TrimSpace(top.Reason)
	if len(reason) > 220 {
		reason = reason[:217] + "..."
	}
	if len(r.CallFailures) == 1 {
		return "every failure was: " + reason
	}
	return fmt.Sprintf("commonest of %d distinct failures (%d of them): %s",
		len(r.CallFailures), top.Count, reason)
}

// MeasureStability compares two distillations of the same URL.
func MeasureStability(a, b *graph.Graph) Stability {
	s := Stability{
		TierA: a.Provenance.Tier, TierB: b.Provenance.Tier,
		TierStable: a.Provenance.Tier == b.Provenance.Tier,
		HashStable: a.ContentHash == b.ContentHash,
		BlocksA:    len(a.Blocks), BlocksB: len(b.Blocks),
	}

	inA := map[string]bool{}
	for _, blk := range a.Blocks {
		inA[blk.Text] = true
	}
	inB := map[string]bool{}
	for _, blk := range b.Blocks {
		inB[blk.Text] = true
	}

	shared := 0
	for t := range inA {
		if inB[t] {
			shared++
		} else if len(s.OnlyInA) < 10 {
			s.OnlyInA = append(s.OnlyInA, truncate(t, 80))
		}
	}
	for t := range inB {
		if !inA[t] && len(s.OnlyInB) < 10 {
			s.OnlyInB = append(s.OnlyInB, truncate(t, 80))
		}
	}
	total := len(inA) + len(inB) - shared
	if total > 0 {
		s.BlockAgreement = round3(float64(shared) / float64(total))
	} else {
		s.BlockAgreement = 1
	}
	sort.Strings(s.OnlyInA)
	sort.Strings(s.OnlyInB)
	return s
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

var _ = context.Background
