package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestHookStaysSilentOnAGoodFetch is the property that decides whether anyone
// keeps this installed.
//
// The hook sits on every WebFetch. If it fires when the fetch worked, it spends
// a browser and tens of seconds to re-read a page the agent already has, and it
// will be uninstalled within a day. Silence is the correct output for a fetch
// that succeeded, and it must cost one string scan to decide.
func TestHookStaysSilentOnAGoodFetch(t *testing.T) {
	real := "Kubernetes, also known as K8s, is an open source system for automating " +
		"deployment, scaling, and management of containerized applications. It groups " +
		"containers that make up an application into logical units for easy management " +
		"and discovery. Kubernetes builds upon 15 years of experience of running " +
		"production workloads at Google, combined with best-of-breed ideas from the community."
	if shell, why := looksLikeShell(real); shell {
		t.Errorf("a real page was judged a shell (%s); this hook would fire on every "+
			"successful fetch and be uninstalled", why)
	}
}

// TestHookFiresOnTheShellsThatMatter covers what WebFetch actually returns when
// it has failed, which is never an error: it is a valid 200 carrying the thing
// standing in front of the page.
func TestHookFiresOnTheShellsThatMatter(t *testing.T) {
	cases := map[string]string{
		"an unhydrated React app":   "You need to enable JavaScript to run this app.",
		"a Cloudflare interstitial": "Just a moment... Checking your browser before accessing.",
		"a bot challenge":           "Attention Required! Please verify you are human.",
		"a policy block":            "Access denied. You do not have permission to view this page.",
		"an empty mount point":      "Loading",
		"a bare title":              "Example Domain",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			shell, why := looksLikeShell(body)
			if !shell {
				t.Errorf("%s was not recognised as a shell, so the agent is left with "+
					"nothing and no signal that anything went wrong", name)
			}
			if why == "" {
				t.Error("fired without saying why; a hook that substitutes its own answer " +
					"for a tool's must explain itself")
			}
		})
	}
}

// TestHookNeverBreaksTheTurn: a hook that fails must not fail the tool call it
// was attached to. WebFetch already returned something, and sieve declining to
// improve on it is not a reason to interrupt a session.
func TestHookNeverBreaksTheTurn(t *testing.T) {
	for _, in := range []string{
		"",                     // nothing on stdin
		"not json at all",      // a payload from a future version
		`{"tool_input":{}}`,    // no url to read
		`{"tool_name":"Bash"}`, // the wrong tool entirely
	} {
		var out, errBuf bytes.Buffer
		code := runHookWith(strings.NewReader(in), &out, &errBuf)
		if code != 0 {
			t.Errorf("input %q exited %d; a hook failure must not fail the turn", in, code)
		}
		if out.Len() != 0 {
			t.Errorf("input %q produced output %q; silence is the correct answer", in, out.String())
		}
	}
}

// TestHookOutputShape checks the envelope Claude Code actually reads.
func TestHookOutputShape(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runHookWith(strings.NewReader(
		`{"tool_name":"WebFetch","tool_input":{"url":"https://example.com"},`+
			`"tool_response":{"result":"You need to enable JavaScript to run this app."}}`),
		&out, &errBuf, "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "https://example.com") {
		t.Errorf("dry run did not name the page it would read: %q", out.String())
	}
}
