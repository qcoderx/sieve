package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qcoderx/sieve/internal/graph"
)

// The benchmark is the instrument every other claim in this project rests on,
// and it had no tests at all. Three separate ways of reporting a result better
// than the truth were found in it by hand in a single afternoon; each would
// return silently the moment someone refactored, and each flattered sieve.
//
// These run against a fake provider rather than a live one, so they are
// deterministic, cost nothing, and can assert on what the model was asked as
// well as on what the harness did with the reply.

// fakeProvider is an OpenAI-compatible endpoint whose replies the test chooses.
type fakeProvider struct {
	srv *httptest.Server
	// reply decides what to answer, given the user turn.
	reply func(userText string) (content string, status int)
	calls atomic.Int64
}

func newFake(t *testing.T, reply func(string) (string, int)) *fakeProvider {
	t.Helper()
	f := &fakeProvider{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &req)
		user := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				user, _ = m.Content.(string)
			}
		}
		content, status := f.reply(user)
		w.Header().Set("Content-Type", "application/json")
		if status != 0 && status != 200 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"forced failure"}}`)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func testSet(n int) *Set {
	s := &Set{URL: "https://example.com/", Name: "test"}
	for i := 0; i < n; i++ {
		s.Questions = append(s.Questions, Question{
			ID:     fmt.Sprintf("q%02d", i+1),
			Band:   BandFactual,
			Ask:    fmt.Sprintf("question %d?", i+1),
			Expect: "the answer",
			Facts:  []string{"the answer"},
		})
	}
	return s
}

// sourceWithText is a served page carrying enough readable text for a fidelity
// check to mean something. Below that floor the check reports that it could not
// run, which is a different answer from a score of zero.
var sourceWithText = "<html><body><p>" +
	strings.Repeat("The workshop was founded in 1974 and has occupied the same rooms since. ", 12) +
	"</p></body></html>"

func testGraph() *graph.Graph {
	return &graph.Graph{
		Title: "Test page",
		Blocks: []graph.Block{
			{ID: "b_000", Type: graph.TypeParagraph, Source: graph.SourceDOM,
				Text: "The workshop was founded in 1974 and the answer is here."},
		},
	}
}

func runWith(t *testing.T, f *fakeProvider, set *Set, raw string) *Report {
	t.Helper()
	r, err := NewRunner(Options{
		BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(context.Background(), Input{Set: set, Artifact: testGraph(), RawHTML: raw})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestFailedCallsAreNotWrongAnswers is the regression guard for the worst of
// the three: a call that never completed was counted as an answer of zero.
//
// The raw page is the control and the larger, more failure-prone request, so
// its collapse was reported as an accuracy gain for the artifact. A run against
// a rate-limited tier announced "+5.0 points" having answered no raw question
// at all.
func TestFailedCallsAreNotWrongAnswers(t *testing.T) {
	// Every raw call fails; every artifact call succeeds and is graded correct.
	f := newFake(t, func(user string) (string, int) {
		if strings.Contains(user, "RAW-PAGE-MARKER") {
			return "", 500
		}
		if strings.Contains(user, "STATEMENTS:") {
			return `{"unsupported": [], "note": "all supported"}`, 200
		}
		if strings.Contains(user, "GIVEN ANSWER") || strings.Contains(user, "grading") {
			return `{"found": ["the answer"], "missed": [], "note": "ok"}`, 200
		}
		return "the answer", 200
	})

	rep := runWith(t, f, testSet(4), "RAW-PAGE-MARKER here is the raw html")

	if rep.Metrics.Raw.Answered != 0 {
		t.Fatalf("raw answered %d, want 0 — the fake failed every raw call", rep.Metrics.Raw.Answered)
	}
	if rep.Metrics.Raw.Errors != 4 {
		t.Errorf("raw errors = %d, want 4", rep.Metrics.Raw.Errors)
	}
	if rep.Verdict.AccuracyGain != 0 {
		t.Errorf("accuracy gain = %+.3f, want 0: a control that answered nothing "+
			"must not hand the artifact a gain", rep.Verdict.AccuracyGain)
	}
	if !strings.Contains(rep.Verdict.Summary, "could not be measured") {
		t.Errorf("verdict = %q, want it to refuse to report a comparison", rep.Verdict.Summary)
	}
	if rep.Verdict.Passed {
		t.Error("a run where the control never answered was marked as passing")
	}
}

// TestAccuracyIsPairedOverCommonQuestions covers the second: subtracting two
// means that describe different question sets.
//
// One side can fail where the other succeeds, and on the fixture the raw side
// answered eleven of twenty against the artifact's twenty. The difference of
// the two means then described nothing at all.
func TestAccuracyIsPairedOverCommonQuestions(t *testing.T) {
	// q01 fails on the raw side only. Raw answers the rest correctly; the
	// artifact answers everything, but wrongly except for q01.
	f := newFake(t, func(user string) (string, int) {
		isRaw := strings.Contains(user, "RAW-PAGE-MARKER")
		switch {
		case strings.Contains(user, "STATEMENTS:"):
			return `{"unsupported": [], "note": "fine"}`, 200
		case strings.Contains(user, "EXPECTED ANSWER"):
			// Grade correct only when the answer says CORRECT.
			if strings.Contains(user, "CORRECT") {
				return `{"found": ["the answer"], "missed": [], "note": "ok"}`, 200
			}
			return `{"found": [], "missed": ["the answer"], "note": "no"}`, 200
		case isRaw && strings.Contains(user, "question 1?"):
			return "", 500
		case isRaw:
			return "CORRECT", 200
		case strings.Contains(user, "question 1?"):
			return "CORRECT", 200
		default:
			return "wrong", 200
		}
	})

	rep := runWith(t, f, testSet(4), "RAW-PAGE-MARKER raw html")

	// Raw answered q02..q04, all correct: mean 1.0 over 3.
	// Artifact answered all four; only q01 correct: mean 0.25 over 4.
	// Unpaired that is 0.25 - 1.0 = -0.75. Paired over q02..q04 it is -1.0,
	// because the artifact got exactly those three wrong.
	if rep.Verdict.ComparedQuestions != 3 {
		t.Fatalf("compared %d questions, want 3 (the ones both sides answered)",
			rep.Verdict.ComparedQuestions)
	}
	// A hair of tolerance: the per-answer accuracies are rounded before they
	// are averaged, so an exact -1 is not guaranteed by the arithmetic.
	if rep.Verdict.AccuracyGain > -0.99 {
		t.Errorf("paired accuracy gain = %+.3f, want -1.000; an unpaired "+
			"subtraction of the means would give -0.750", rep.Verdict.AccuracyGain)
	}
}

// TestFidelityCountsFromIndices covers the check that gates the release.
//
// It used to ask the grader to echo unsupported statements verbatim into a
// bounded reply, so on a page with real content the JSON was truncated
// mid-string and fidelity reported "could not be measured" — silently
// removing the one criterion that catches invention.
func TestFidelityCountsFromIndices(t *testing.T) {
	f := newFake(t, func(user string) (string, int) {
		switch {
		case strings.Contains(user, "STATEMENTS:"):
			return "```json\n{\"unsupported\": [0], \"note\": \"one invented\"}\n```", 200
		case strings.Contains(user, "EXPECTED ANSWER"):
			return `{"found": ["the answer"], "missed": [], "note": "ok"}`, 200
		default:
			return "the answer", 200
		}
	})

	g := testGraph()
	g.Blocks = append(g.Blocks,
		graph.Block{ID: "b_001", Type: graph.TypeParagraph, Source: graph.SourceDOM,
			Text: "A second statement long enough to be sampled by the checker."},
	)
	r, err := NewRunner(Options{BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(context.Background(), Input{
		Set: testSet(1), Artifact: g, RawHTML: sourceWithText})
	if err != nil {
		t.Fatal(err)
	}

	// One of two statements unsupported.
	if rep.Fidelity != 0.5 {
		t.Errorf("fidelity = %.3f, want 0.500 (one of two statements unsupported)", rep.Fidelity)
	}
	joined := strings.Join(rep.FidelityNotes, " ")
	if !strings.Contains(joined, "unsupported:") {
		t.Errorf("the unsupported statement was not named in the notes: %v", rep.FidelityNotes)
	}
}

// TestFidelityIgnoresOutOfRangeIndices: a grader naming statement 99 of a
// sample of two is hallucinating, and must not be able to move the score.
func TestFidelityIgnoresOutOfRangeIndices(t *testing.T) {
	f := newFake(t, func(user string) (string, int) {
		switch {
		case strings.Contains(user, "STATEMENTS:"):
			return `{"unsupported": [99, 0, 0], "note": "confused"}`, 200
		case strings.Contains(user, "EXPECTED ANSWER"):
			return `{"found": [], "missed": ["the answer"], "note": "no"}`, 200
		default:
			return "an answer", 200
		}
	})
	r, _ := NewRunner(Options{BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1})
	rep, err := r.Run(context.Background(), Input{
		Set: testSet(1), Artifact: testGraph(), RawHTML: sourceWithText})
	if err != nil {
		t.Fatal(err)
	}
	// One real statement, named unsupported once (the duplicate and the out of
	// range index are both discarded): fidelity 0.
	if rep.Fidelity != 0 {
		t.Errorf("fidelity = %.3f, want 0.000", rep.Fidelity)
	}
	if !strings.Contains(strings.Join(rep.FidelityNotes, " "), "outside the sample") {
		t.Errorf("the bogus index was not reported: %v", rep.FidelityNotes)
	}
}

// TestBothConditionsGetTheSamePrompt is the fairness check.
//
// The whole comparison rests on the only difference between the two being what
// the agent was given to read. A friendlier prompt for the artifact would be
// the easiest possible thumb on the scale, and the least visible.
func TestBothConditionsGetTheSamePrompt(t *testing.T) {
	var prompts []string
	f := newFake(t, func(user string) (string, int) {
		if strings.Contains(user, "STATEMENTS:") || strings.Contains(user, "EXPECTED ANSWER") {
			return `{"unsupported": [], "found": [], "missed": [], "note": "x"}`, 200
		}
		prompts = append(prompts, user)
		return "answer", 200
	})
	runWith(t, f, testSet(1), "RAW-PAGE-MARKER")

	if len(prompts) != 2 {
		t.Fatalf("got %d answering prompts, want 2", len(prompts))
	}
	// Strip the page text; what surrounds it must be identical.
	shape := func(s string) string {
		if i := strings.Index(s, ">>>"); i >= 0 {
			return s[i:]
		}
		return s
	}
	if shape(prompts[0]) != shape(prompts[1]) {
		t.Errorf("the two conditions were asked differently:\n raw=%q\n art=%q",
			shape(prompts[0]), shape(prompts[1]))
	}
}

// TestArtifactConditionCarriesActionsAndForms guards the redaction bug: the
// artifact condition was given a rendering with actions, forms and navigation
// stripped out, then measured on questions about forms.
func TestArtifactConditionCarriesActionsAndForms(t *testing.T) {
	var artPrompt string
	f := newFake(t, func(user string) (string, int) {
		if strings.Contains(user, "STATEMENTS:") || strings.Contains(user, "EXPECTED ANSWER") {
			return `{"unsupported": [], "found": [], "missed": [], "note": "x"}`, 200
		}
		if !strings.Contains(user, "RAW-PAGE-MARKER") {
			artPrompt = user
		}
		return "answer", 200
	})

	g := testGraph()
	g.Actions = []graph.Action{{
		ID: "a_000", Type: "form", Label: "Enquiry",
		Href: "https://example.com/enquiry", Method: "POST",
		Fields: []graph.Field{{Name: "email", Type: "email", Required: true}},
	}}
	r, _ := NewRunner(Options{BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1})
	if _, err := r.Run(context.Background(), Input{
		Set: testSet(1), Artifact: g, RawHTML: "RAW-PAGE-MARKER"}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"POST", "enquiry", "email"} {
		if !strings.Contains(artPrompt, want) {
			t.Errorf("the artifact condition was not given %q; it cannot answer "+
				"a question about the form it was never shown", want)
		}
	}
}

// TestCoverageMatching pins the fact matcher, including the inflection case.
func TestCoverageMatching(t *testing.T) {
	page := strings.ToLower(
		"## The studio\nSubmits `POST` to https://example.com/enquiry\n" +
			"Portuguese cork, from Alentejo\nEleven people work here.")
	cases := []struct {
		fact string
		want bool
		why  string
	}{
		{"eleven people work here", true, "stated almost verbatim"},
		{"it posts to /enquiry", true, "posts and POST are the same word"},
		{"cork from Alentejo", true, "distinctive words all present"},
		{"the studio", true, "a heading is text"},
		{"founded in 1974 by Ines Aurelia", false, "nothing about it on the page"},
		{"a dining table takes nine weeks", false, "absent"},
	}
	for _, c := range cases {
		if got := factPresent(page, c.fact); got != c.want {
			t.Errorf("factPresent(%q) = %v, want %v — %s", c.fact, got, c.want, c.why)
		}
	}
}

// TestRawIsTruncatedToContext covers the case the benchmark previously could
// not measure at all: a page too large to send.
//
// Without a cap the control errors on anything big, so every result came from
// small pages -- precisely the ones where distillation matters least. An agent
// handed a four-hundred-thousand-token page does not read it all either, so
// truncating models reality rather than dodging it. What must not happen is
// doing it quietly: the report has to carry both figures.
func TestRawIsTruncatedToContext(t *testing.T) {
	var rawSeen string
	f := newFake(t, func(user string) (string, int) {
		switch {
		case strings.Contains(user, "STATEMENTS:"):
			return `{"unsupported": [], "note": "x"}`, 200
		case strings.Contains(user, "EXPECTED ANSWER"):
			return `{"found": [], "missed": ["the answer"], "note": "x"}`, 200
		}
		if strings.Contains(user, "RAWMARK") {
			rawSeen = user
		}
		return "answer", 200
	})

	huge := "RAWMARK " + strings.Repeat("filler text that goes on and on. ", 20000)
	r, err := NewRunner(Options{
		BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1,
		RawContextTokens: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(context.Background(), Input{
		Set: testSet(1), Artifact: testGraph(), RawHTML: huge})
	if err != nil {
		t.Fatal(err)
	}

	if !rep.RawTruncated {
		t.Error("a page far over the cap was not marked truncated")
	}
	if rep.RawTokens <= rep.RawTokensSent {
		t.Errorf("raw tokens %d and sent %d: the report does not show what was withheld",
			rep.RawTokens, rep.RawTokensSent)
	}
	if !strings.Contains(rawSeen, "truncated") {
		t.Error("the control was not told its page had been cut short")
	}
	if len(rawSeen) >= len(huge) {
		t.Error("the whole page was sent despite the cap")
	}
}

// TestRawUntouchedWhenItFits: a page inside the cap must arrive whole, or the
// common case would be quietly damaged by a guard meant for the rare one.
func TestRawUntouchedWhenItFits(t *testing.T) {
	f := newFake(t, func(user string) (string, int) {
		if strings.Contains(user, "STATEMENTS:") || strings.Contains(user, "EXPECTED ANSWER") {
			return `{"unsupported": [], "found": [], "missed": [], "note": "x"}`, 200
		}
		return "answer", 200
	})
	r, _ := NewRunner(Options{
		BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1,
		RawContextTokens: 100_000,
	})
	rep, err := r.Run(context.Background(), Input{
		Set: testSet(1), Artifact: testGraph(), RawHTML: "a short page"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RawTruncated {
		t.Error("a short page was reported as truncated")
	}
}

// TestGraderAgreementIsMeasured: every accuracy figure is the grader's
// opinion, and an opinion that changes between two identical questions is
// noise reported as a measurement. This asks how wide that noise is.
func TestGraderAgreementIsMeasured(t *testing.T) {
	var grades int
	f := newFake(t, func(user string) (string, int) {
		switch {
		case strings.Contains(user, "STATEMENTS:"):
			return `{"unsupported": [], "note": "x"}`, 200
		case strings.Contains(user, "EXPECTED ANSWER"):
			grades++
			// Disagree with itself on every second grading.
			if grades%2 == 0 {
				return `{"found": ["the answer"], "missed": [], "note": "yes"}`, 200
			}
			return `{"found": [], "missed": ["the answer"], "note": "no"}`, 200
		}
		return "answer", 200
	})
	r, _ := NewRunner(Options{
		BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1,
		GraderRepeats: 4,
	})
	rep, err := r.Run(context.Background(), Input{
		Set: testSet(4), Artifact: testGraph(), RawHTML: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.GraderRegraded == 0 {
		t.Fatal("no answers were re-graded despite GraderRepeats being set")
	}
	// A grader that alternates cannot agree with itself all the time; the point
	// is that the disagreement is now visible rather than assumed away.
	if rep.GraderAgreement == 1 {
		t.Errorf("agreement reported as perfect from a deliberately inconsistent grader")
	}
}

// TestFidelityUnmeasurableWhenSourceIsEmpty covers the case that condemned
// sieve for doing its job.
//
// Fidelity verifies the artifact's statements against the served HTML. On a
// site that renders its content some other way that HTML is a shell: igloo.inc
// serves an empty body, so every correctly extracted sentence came back "not
// found in source" and the gate criterion read 0.083 for an artifact that had
// invented nothing at all. A score of zero and a check that could not run are
// opposite conclusions and must not share a number.
func TestFidelityUnmeasurableWhenSourceIsEmpty(t *testing.T) {
	f := newFake(t, func(user string) (string, int) {
		if strings.Contains(user, "STATEMENTS:") {
			t.Error("the fidelity grader was called even though the source has no text")
			return `{"unsupported": [0], "note": "x"}`, 200
		}
		if strings.Contains(user, "EXPECTED ANSWER") {
			return `{"found": ["the answer"], "missed": [], "note": "ok"}`, 200
		}
		return "the answer", 200
	})

	r, _ := NewRunner(Options{BaseURL: f.srv.URL, APIKey: "k", Model: "fake", Concurrency: 1})
	rep, err := r.Run(context.Background(), Input{
		Set: testSet(2), Artifact: testGraph(),
		// An application shell: markup and script, no prose.
		RawHTML: `<html><head><title>Igloo</title></head><body></body></html>`,
	})
	if err != nil {
		t.Fatal(err)
	}

	if rep.FidelityMeasured {
		t.Error("fidelity was reported as measured against a page with no text in it")
	}
	joined := strings.Join(rep.FidelityNotes, " ")
	if !strings.Contains(joined, "nothing to verify") {
		t.Errorf("the reason was not given: %v", rep.FidelityNotes)
	}
	if !strings.Contains(rep.Verdict.Summary, "not measured") {
		t.Errorf("the verdict blamed the artifact rather than the missing source: %q",
			rep.Verdict.Summary)
	}
}
