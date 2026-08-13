package graph

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Status is what happened when sieve tried to read the page.
//
// It exists because the worst failure in this category is an agent that cannot
// tell reading failed. A bot challenge, a login wall and an unhydrated
// single-page shell all arrive as a valid HTTP 200 carrying valid HTML and no
// content, and an agent handed that either reports the page as empty or invents
// something to fill the gap. sieve already wrote the evidence into prose notes,
// which a human reads and a program cannot: an artifact for a 403 carried the
// number 403 exactly once, inside a Chrome error string, with no field anywhere
// saying the request had been refused.
//
// So this is deliberately a small closed set, checked before the content is
// read, and every value carries the evidence that produced it.
type Status string

const (
	// StatusOK: content was extracted normally.
	StatusOK Status = "ok"
	// StatusBlocked: the site refused -- HTTP 4xx or 5xx, robots.txt, or a rate
	// limit. The artifact describes the refusal, not the page.
	StatusBlocked Status = "blocked"
	// StatusChallenge: a bot-protection interstitial answered instead of the
	// page. Distinct from blocked because it is often passable by other means
	// and is not a policy decision about this client.
	StatusChallenge Status = "challenge"
	// StatusAuthRequired: a login wall stands in front of the content.
	StatusAuthRequired Status = "auth_required"
	// StatusSPAShell: the served document is an unhydrated shell and the
	// browser did not fill it in. This is the failure that most looks like
	// success: valid markup, correct status code, nothing in it.
	StatusSPAShell Status = "spa_shell"
	// StatusEmptyAfterRender: the page rendered and genuinely has no extractable
	// text. A real answer, and a different one from every case above.
	StatusEmptyAfterRender Status = "empty_after_render"
	// StatusPartial: content was extracted, but something was reached for and
	// missed -- a sweep cut short, an entry screen never passed, a tier that
	// failed and fell back.
	StatusPartial Status = "partial"
)

// Outcome is the machine-readable verdict, with its evidence.
type Outcome struct {
	Status Status `json:"status"`
	// Evidence lists what led to the status, shortest first. It is never empty
	// for a status other than ok: a verdict a caller cannot check is a verdict
	// they have to trust.
	Evidence []string `json:"evidence,omitempty"`

	// HTTPStatus is the code the server answered with, recorded whatever it
	// was. An agent that knows a read failed because of a 429 stops retrying;
	// one that only knows the page was empty keeps going.
	HTTPStatus int `json:"http_status,omitempty"`
	// BodyExcerpt is the beginning of the response body on an error, which is
	// where a proxy or policy filter says who blocked the request and why.
	BodyExcerpt string `json:"body_excerpt,omitempty"`
}

// Failed reports whether the read did not produce the page that was asked for.
func (o Outcome) Failed() bool {
	return o.Status != StatusOK && o.Status != StatusPartial
}

// maxBodyExcerpt bounds what an error body contributes to the artifact.
//
// Long enough to carry "Access denied: your IP is not permitted by policy X",
// which is the whole reason for keeping it, and short enough that a server
// answering an error with a full HTML page cannot spend a caller's context on
// it.
const maxBodyExcerpt = 400

// ellipsis marks a truncated excerpt.
const ellipsis = "…"

// OutcomeInput is what deciding a status needs. It is separate from the graph
// because most of it is known before the graph exists.
type OutcomeInput struct {
	HTTPStatus    int
	Body          string
	Blocked       bool
	BlockedReason string
	// RobotsRefused records a robots.txt disallow, which is a refusal by policy
	// rather than by the server.
	RobotsRefused bool
	// EntryGate is an interstitial that was found and not passed.
	EntryGate string
	// ShellHTML reports that the served document was judged a shell.
	ShellHTML bool
	// Rendered reports that a browser ran the page. Without it, an empty result
	// cannot be called empty-after-render.
	Rendered bool
	// TierFellBack reports that a higher tier was chosen and did not deliver.
	TierFellBack bool
	// TierReason explains that fallback, and is used as the evidence for it.
	TierReason string
	// SweepTruncated reports that the sweep did not finish the document.
	//
	// It is evidence rather than a verdict. Long pages routinely end a sweep
	// before the bottom, so a status driven by it would mark most of the web
	// partial and mean nothing; it is recorded when the artifact is partial for
	// a reason that does distinguish it.
	SweepTruncated bool
}

// DecideOutcome derives the status from the signals and the finished graph.
//
// The order is a precedence, not a sequence of guesses: a page that is both
// refused and empty is refused, because that is the fact that explains the
// other one and the one a caller must act on.
func DecideOutcome(in OutcomeInput, contentBlocks int) Outcome {
	o := Outcome{HTTPStatus: in.HTTPStatus}
	if in.HTTPStatus >= 400 {
		o.BodyExcerpt = excerpt(in.Body)
	}

	add := func(f string, a ...any) { o.Evidence = append(o.Evidence, fmt.Sprintf(f, a...)) }

	switch {
	case in.RobotsRefused:
		o.Status = StatusBlocked
		add("robots.txt disallows this path for this user agent")

	case in.HTTPStatus == 401 || in.HTTPStatus == 407:
		o.Status = StatusAuthRequired
		add("the server answered HTTP %d", in.HTTPStatus)

	case looksLikeChallenge(in.BlockedReason):
		o.Status = StatusChallenge
		add("%s", in.BlockedReason)

	case in.HTTPStatus >= 400:
		o.Status = StatusBlocked
		add("the server answered HTTP %d", in.HTTPStatus)
		if in.BlockedReason != "" {
			add("%s", in.BlockedReason)
		}

	case in.Blocked:
		o.Status = StatusBlocked
		if in.BlockedReason != "" {
			add("%s", in.BlockedReason)
		} else {
			add("the site refused this client")
		}

	case in.EntryGate != "":
		// An interstitial that was never passed means the artifact describes the
		// screen rather than the site, however much text the screen carried.
		o.Status = StatusChallenge
		add("an entry screen labelled %q was not passed", in.EntryGate)

	case contentBlocks == 0 && in.ShellHTML && !in.Rendered:
		o.Status = StatusSPAShell
		add("the served document is a shell and no browser rendered it")

	case contentBlocks == 0 && in.ShellHTML:
		o.Status = StatusSPAShell
		add("the served document is a shell and rendering it produced no text")

	case contentBlocks == 0 && in.Rendered:
		o.Status = StatusEmptyAfterRender
		add("the page rendered and carried no extractable text")

	case contentBlocks == 0:
		o.Status = StatusEmptyAfterRender
		add("no extractable text was found in the served document")

	case in.TierFellBack:
		o.Status = StatusPartial
		if in.TierReason != "" {
			add("%s", in.TierReason)
		} else {
			add("a higher tier was chosen and did not deliver, so a cheaper one was used")
		}
		if in.SweepTruncated {
			add("the sweep did not reach the bottom of the document")
		}

	default:
		o.Status = StatusOK
	}
	return o
}

// challengeMarkers are the phrases a bot-protection interstitial identifies
// itself by. They are matched against sieve's own reason string rather than
// against page text, so a page merely discussing Cloudflare is not a challenge.
var challengeMarkers = []string{
	"cloudflare", "challenge", "captcha", "are you human",
	"just a moment", "attention required", "ddos", "bot protection",
}

func looksLikeChallenge(reason string) bool {
	if reason == "" {
		return false
	}
	r := strings.ToLower(reason)
	for _, m := range challengeMarkers {
		if strings.Contains(r, m) {
			return true
		}
	}
	return false
}

func excerpt(body string) string {
	s := strings.TrimSpace(body)
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxBodyExcerpt {
		return s
	}
	// Budget in bytes, because that is what the cap means, and back off to a
	// rune boundary so the excerpt cannot end in half a character.
	cut := maxBodyExcerpt - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
