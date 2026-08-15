package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/safety"
)

// The hook exists because the detection half of this problem is already solved
// and published, and the fallback half is the part nobody had.
//
// Claude Code's WebFetch has no JavaScript engine. On a React or Next.js page it
// retrieves the pre-render shell: a valid 200, valid HTML, no content. The agent
// gets no signal that the fetch failed, so it either reports the page as empty
// or invents something to fill the gap. Community hooks already spot this by
// sniffing for a noscript tag, an empty mount point or a challenge marker, and
// then suggest reaching for a browser MCP by hand.
//
// This closes the loop. When WebFetch comes back with a shell, sieve reads the
// page properly and hands the result back as context, in the same turn, without
// anyone deciding to adopt an extraction tool.
//
// It fires only on failure. A WebFetch that worked is left alone: the cost of
// this hook on an ordinary page must be one string scan, because a hook that
// taxes every fetch is a hook people uninstall.

// hookInput is the payload Claude Code writes to a PostToolUse hook's stdin.
type hookInput struct {
	SessionID string `json:"session_id"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		URL    string `json:"url"`
		Prompt string `json:"prompt"`
	} `json:"tool_input"`
	// ToolResponse is whatever WebFetch produced. Its shape is not contractual,
	// so it is read as raw JSON and searched as text rather than unmarshalled
	// into a struct that a future version could invalidate.
	ToolResponse json.RawMessage `json:"tool_response"`
}

// hookOutput is what Claude Code reads back from stdout.
type hookOutput struct {
	SystemMessage      string              `json:"systemMessage,omitempty"`
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// shellMarkers are the phrases a page shows when its content did not arrive.
//
// Matched against what WebFetch returned, not against raw HTML, because by the
// time a hook sees it the response has already been converted to Markdown and
// the tags are gone. What survives that conversion is the visible text, which
// is exactly what these pages put on screen.
var shellMarkers = []string{
	"you need to enable javascript",
	"enable javascript to run this app",
	"please enable javascript",
	"javascript is required",
	"this site requires javascript",
	"just a moment",
	"checking your browser",
	"verifying you are human",
	"enable cookies and reload",
	"access denied",
	"attention required",
}

// emptyShellChars is how little readable text a real page may return before the
// response is treated as a shell.
//
// Deliberately low. A short page is common and a hook that fires on every terse
// document would spend a browser on nothing; a response under this is not a
// short page, it is a mount point and a loading spinner.
const emptyShellChars = 220

var wordish = regexp.MustCompile(`[A-Za-z][A-Za-z'-]+`)

// looksLikeShell decides whether WebFetch came back with the page or with the
// thing standing in front of it.
//
// It reports the reason as well as the verdict, because a hook that silently
// substitutes its own answer for a tool's is worse than one that says why.
func looksLikeShell(body string) (bool, string) {
	low := strings.ToLower(body)
	for _, m := range shellMarkers {
		if strings.Contains(low, m) {
			return true, fmt.Sprintf("the response says %q, which is what a page shows when its content has not arrived", m)
		}
	}
	// Words rather than characters: a response that is mostly a URL, a nav list
	// or a cookie notice can be long and still say nothing.
	if n := len(wordish.FindAllString(body, -1)); n*6 < emptyShellChars {
		return true, fmt.Sprintf("the response carried %d words, which is a mount point rather than a document", n)
	}
	return false, ""
}

// runHook reads the payload from stdin. runHookWith exists so tests can supply
// one without a process.
func runHook(args []string, stdout, stderr io.Writer) int {
	return runHookWith(os.Stdin, stdout, stderr, args...)
}

func runHookWith(stdin io.Reader, stdout, stderr io.Writer, args ...string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		common  commonFlags
		dryRun  bool
		maxWait time.Duration
	)
	common.register(fs)
	fs.BoolVar(&dryRun, "dry-run", false,
		"report the decision and do not distill. For checking the hook is wired up")
	fs.DurationVar(&maxWait, "max-wait", 45*time.Second,
		"give up rather than hold up the turn. A hook that stalls a session is worse\n"+
			"than one that misses a page")

	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: sieve hook [flags]

Reads a Claude Code PostToolUse payload on stdin. When WebFetch came back with a
shell rather than the page, reads the page properly and returns the content as
additional context. Otherwise does nothing.

Wire it up in .claude/settings.json:

  {
    "hooks": {
      "PostToolUse": [{
        "matcher": "WebFetch",
        "hooks": [{"type": "command", "command": "sieve hook", "timeout": 60}]
      }]
    }
  }

Flags:
`)
		fs.PrintDefaults()
	}
	if _, err := parseArgs(fs, args); err != nil {
		return 2
	}

	raw, err := io.ReadAll(io.LimitReader(stdin, 8<<20))
	if err != nil {
		return hookQuiet(stderr, "could not read the hook payload: %v", err)
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return hookQuiet(stderr, "could not parse the hook payload: %v", err)
	}
	// A hook that fires on the wrong tool, or with no URL to read, does nothing
	// and says nothing. Silence is the correct output for "not my business".
	if in.ToolInput.URL == "" {
		return 0
	}

	shell, why := looksLikeShell(string(in.ToolResponse))
	if !shell {
		return 0
	}
	if dryRun {
		fmt.Fprintf(stdout, "sieve would read %s: %s\n", in.ToolInput.URL, why)
		return 0
	}

	body, oc, derr := hookDistill(in.ToolInput.URL, common, maxWait)
	if derr != nil {
		// The hook failed and the turn carries on. Reporting the failure as
		// context would put an error message where page content should be.
		return hookQuiet(stderr, "sieve could not read %s: %v", in.ToolInput.URL, derr)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "WebFetch returned a shell for %s: %s\n\n", in.ToolInput.URL, why)
	if oc.Status != graph.StatusOK {
		fmt.Fprintf(&b, "sieve read it and the page did not arrive either. Outcome: %s.\n", oc.Status)
		for _, e := range oc.Evidence {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
		b.WriteString("\nDo not describe this page as empty, and do not fill the gap with " +
			"what you expect it to say.\n")
	} else {
		b.WriteString("sieve read the page with a browser. Its content follows, and it is " +
			"the page WebFetch could not see.\n\n")
		b.WriteString(body)
	}

	out := hookOutput{
		SystemMessage: fmt.Sprintf("sieve read %s (WebFetch got a shell)", in.ToolInput.URL),
		HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "PostToolUse",
			AdditionalContext: b.String(),
		},
	}
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(out); err != nil {
		return hookQuiet(stderr, "could not write the hook result: %v", err)
	}
	return 0
}

// hookDistill reads the page and renders what an agent should see.
func hookDistill(target string, common commonFlags, maxWait time.Duration) (string, graph.Outcome, error) {
	opts := distill.DefaultOptions()
	opts.Guard = common.guard()
	opts.Limiter = common.limiter()
	opts.Memory = loadMemory(common.memoryPath)
	opts.Robots = safety.NewRobotsCache(nil)
	opts.Render.ChromePath = common.chrome

	// The turn is waiting. Whatever the operator's usual budgets are, a hook
	// gets the smaller of them and its own ceiling.
	if maxWait > 0 {
		if common.timeout > maxWait {
			opts.Render.ScaleTo(maxWait)
		} else {
			opts.Render.ScaleTo(common.timeout)
		}
		if common.loadTimeout > maxWait {
			opts.Render.LoadBudget = maxWait
		} else {
			opts.Render.LoadBudget = common.loadTimeout
		}
	}

	d := distill.New(opts)
	defer d.Close()

	ctx, cancel := withTimeout(maxWait)
	defer cancel()

	res, err := d.Distill(ctx, target)
	if err != nil {
		return "", graph.Outcome{}, err
	}
	opt := emit.CompactMarkdownOptions()
	opt.Actions, opt.Navigation, opt.Structured, opt.Gaps, opt.Notes = true, true, true, true, true
	return emit.Markdown(res.Graph, opt), res.Graph.Outcome, nil
}

// hookQuiet reports a problem to the transcript and exits successfully.
//
// A hook that fails must not fail the tool call it was attached to. WebFetch
// already returned something; sieve declining to improve on it is not a reason
// to interrupt the turn.
func hookQuiet(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "sieve hook: "+format+"\n", a...)
	return 0
}
