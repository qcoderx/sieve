package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveOnce stands in for a provider and hands back the request it was given,
// so the tests can assert on the wire format rather than on our intentions.
func serveOnce(t *testing.T, status int, body string, captured *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if m == nil {
				m = map[string]any{}
			}
			m["_path"] = r.URL.Path
			m["_auth"] = r.Header.Get("Authorization")
			*captured = m
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const okBody = `{"choices":[{"message":{"content":" Paris "},"finish_reason":"stop"}],
	"usage":{"prompt_tokens":87,"completion_tokens":3,
	"prompt_tokens_details":{"cached_tokens":12}}}`

// TestOpenAIShape covers the request a compatible provider actually receives.
//
// The point of this path is that one wire format reaches every provider that
// is not Anthropic, so the parts that must be right are the ones a provider
// would reject: the endpoint, the bearer header, and a text-only turn sent as
// a plain string rather than a parts array, which some implementations do not
// accept.
func TestOpenAIShape(t *testing.T) {
	var got map[string]any
	srv := serveOnce(t, 200, okBody, &got)

	c, err := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "some-model", MaxTokens: 64})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Ask(context.Background(), "be terse", []ContentBlock{TextBlock("hi")})
	if err != nil {
		t.Fatal(err)
	}

	if got["_path"] != "/chat/completions" {
		t.Errorf("posted to %v, want /chat/completions", got["_path"])
	}
	if got["_auth"] != "Bearer k" {
		t.Errorf("auth header = %v", got["_auth"])
	}
	if got["model"] != "some-model" {
		t.Errorf("model = %v", got["model"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want system + user", len(msgs))
	}
	user, _ := msgs[1].(map[string]any)
	if _, isString := user["content"].(string); !isString {
		t.Errorf("a text-only turn was sent as %T, want a plain string; "+
			"some providers reject a parts array", user["content"])
	}

	// The answer is trimmed, and usage comes from the provider's accounting
	// with any cached prefix kept separate so the benchmark stays honest.
	if res.Text != "Paris" {
		t.Errorf("text = %q, want %q", res.Text, "Paris")
	}
	if res.Usage.InputTokens != 75 || res.Usage.CacheReadTokens != 12 {
		t.Errorf("usage in=%d cached=%d, want 75 and 12",
			res.Usage.InputTokens, res.Usage.CacheReadTokens)
	}
	if res.Usage.OutputTokens != 3 || res.Usage.Calls != 1 {
		t.Errorf("usage out=%d calls=%d", res.Usage.OutputTokens, res.Usage.Calls)
	}
}

// TestOpenAIImageTurn checks that an image forces the parts array, since that
// is the only form that can carry one.
func TestOpenAIImageTurn(t *testing.T) {
	var got map[string]any
	srv := serveOnce(t, 200, okBody, &got)

	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	if _, err := c.Ask(context.Background(), "", []ContentBlock{
		ImageBlock("image/png", "AAAA"),
		TextBlock("what is this"),
	}); err != nil {
		t.Fatal(err)
	}

	msgs, _ := got["messages"].([]any)
	user, _ := msgs[0].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("an image turn was sent as %T, want a parts array", user["content"])
	}
	first, _ := parts[0].(map[string]any)
	if first["type"] != "image_url" {
		t.Errorf("first part type = %v", first["type"])
	}
	img, _ := first["image_url"].(map[string]any)
	if u, _ := img["url"].(string); !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Errorf("image url = %q, want a data URL", u)
	}
}

// TestOpenAIErrors keeps failures legible. A provider's own message is worth
// more than a status code, and a 401 has to arrive as ErrNoCredentials so the
// caller prints the setup instructions rather than a bare number.
func TestOpenAIErrors(t *testing.T) {
	t.Run("401 becomes ErrNoCredentials", func(t *testing.T) {
		srv := serveOnce(t, 401, `{"error":{"message":"bad key"}}`, nil)
		c, _ := New(Options{BaseURL: srv.URL, Model: "m"})
		_, err := c.Ask(context.Background(), "", []ContentBlock{TextBlock("x")})
		if err == nil || !strings.Contains(err.Error(), "no model credentials") {
			t.Errorf("err = %v, want the credentials guidance", err)
		}
	})

	t.Run("the provider's message survives", func(t *testing.T) {
		srv := serveOnce(t, 400, `{"error":{"message":"model_not_found: nope"}}`, nil)
		c, _ := New(Options{BaseURL: srv.URL, Model: "nope"})
		_, err := c.Ask(context.Background(), "", []ContentBlock{TextBlock("x")})
		if err == nil || !strings.Contains(err.Error(), "model_not_found") {
			t.Errorf("err = %v, want the provider's own message", err)
		}
	})

	t.Run("a non-JSON body does not panic", func(t *testing.T) {
		srv := serveOnce(t, 502, `<html>bad gateway</html>`, nil)
		c, _ := New(Options{BaseURL: srv.URL, Model: "m"})
		if _, err := c.Ask(context.Background(), "", []ContentBlock{TextBlock("x")}); err == nil {
			t.Error("want an error for a non-JSON body")
		}
	})

	t.Run("a refusal is a result, not an error", func(t *testing.T) {
		srv := serveOnce(t, 200,
			`{"choices":[{"message":{"content":"","refusal":"no"},"finish_reason":"stop"}],"usage":{}}`, nil)
		c, _ := New(Options{BaseURL: srv.URL, Model: "m"})
		res, err := c.Ask(context.Background(), "", []ContentBlock{TextBlock("x")})
		if err != nil {
			t.Fatalf("a refusal should not be an error: %v", err)
		}
		if !res.Refused {
			t.Error("refusal not reported")
		}
	})
}

// TestBaseURLNeedsAModel: there is no sensible default model name outside
// Anthropic, and guessing one produces a 404 that reads like a broken build.
func TestBaseURLNeedsAModel(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://example.invalid/v1"}); err == nil {
		t.Error("a base URL with no model was accepted")
	}
}

// TestReasoningWithoutAnswerIsAnError covers a failure that would otherwise be
// scored as a wrong answer.
//
// A thinking model returns its answer in content and its working in a separate
// field. When the token ceiling is reached mid-thought, content is empty and
// only the working comes back. Handing that to a grader marks the question
// wrong, when in truth the model never answered — the same mistake as counting
// a failed call as a wrong answer, and just as flattering to whichever
// condition happens to survive it.
func TestReasoningWithoutAnswerIsAnError(t *testing.T) {
	srv := serveOnce(t, 200, `{"choices":[{"message":{"content":"",
		"reasoning":"We are asked for the capital. Let me think about France..."},
		"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`, nil)

	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "thinker", MaxTokens: 20})
	_, err := c.Ask(context.Background(), "", []ContentBlock{TextBlock("capital of France?")})
	if err == nil {
		t.Fatal("an empty answer from a thinking model was returned as if it were a reply")
	}
	if !strings.Contains(err.Error(), "output tokens") {
		t.Errorf("err = %v, want it to name the cause and the remedy", err)
	}
}

// TestReasoningAlongsideAnAnswerIsFine: when the model did reach a conclusion,
// the working is ignored and the answer is used.
func TestReasoningAlongsideAnAnswerIsFine(t *testing.T) {
	srv := serveOnce(t, 200, `{"choices":[{"message":{"content":"Paris",
		"reasoning":"France's capital is Paris."},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5}}`, nil)

	c, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "thinker"})
	res, err := c.Ask(context.Background(), "", []ContentBlock{TextBlock("capital?")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Paris" {
		t.Errorf("text = %q, want %q — the working must not displace the answer", res.Text, "Paris")
	}
}

// TestProviderErrorShapes: the message a provider sends must survive, whichever
// shape it uses.
//
// OpenAI and most imitators wrap it in an object; HuggingFace returns a bare
// string. A client that understands only the object form reports "a body that
// is not JSON" while the provider is plainly saying something useful — in the
// case that prompted this, that the account had run out of credit, which is a
// two-second fix misread as a broken integration.
func TestProviderErrorShapes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"object form", 400,
			`{"error":{"message":"model_not_found: nope","type":"invalid"}}`, "model_not_found"},
		{"bare string form", 402,
			`{"error":"You have depleted your monthly included credits."}`, "depleted"},
		{"payment required is named", 402,
			`{"error":"out of credit"}`, "payment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serveOnce(t, c.status, c.body, nil)
			cl, _ := New(Options{BaseURL: srv.URL, APIKey: "k", Model: "m"})
			_, err := cl.Ask(context.Background(), "", []ContentBlock{TextBlock("x")})
			if err == nil {
				t.Fatal("no error returned")
			}
			if !strings.Contains(strings.ToLower(err.Error()), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
