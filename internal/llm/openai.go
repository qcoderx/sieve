package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// openAIClient speaks the OpenAI chat completions shape.
//
// That shape is the lingua franca: Groq, OpenRouter, Together, DeepSeek,
// Fireworks, vLLM, LM Studio and Ollama all expose it, so supporting this one
// wire format is what lets a user point sieve at whatever model they have.
//
// It is written against net/http rather than pulling in another SDK. The
// request is a single non-streaming turn with a token cap, the response is one
// choice and a usage block, and every provider that claims compatibility
// implements exactly that much. A dependency would buy nothing here and would
// tie the project to one vendor's idea of the same endpoint.
type openAIClient struct {
	base string
	key  string
	http *http.Client
}

func newOpenAI(base, key string, timeout time.Duration) *openAIClient {
	return &openAIClient{
		base: strings.TrimRight(base, "/"),
		key:  key,
		http: &http.Client{Timeout: timeout},
	}
}

// oaMessage is one turn. Content is either a plain string or an array of parts,
// and providers vary in which they accept, so a text-only turn is sent as a
// plain string -- the form every implementation handles.
type oaMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type oaPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *oaImgURL `json:"image_url,omitempty"`
}

type oaImgURL struct {
	URL string `json:"url"`
}

type oaRequest struct {
	Model     string      `json:"model"`
	Messages  []oaMessage `json:"messages"`
	MaxTokens int64       `json:"max_tokens,omitempty"`
}

type oaResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
			// Reasoning is where a thinking model puts its working. It is not
			// the answer and is never used as one; it is read only to tell an
			// empty reply that ran out of budget mid-thought apart from an
			// empty reply that had nothing to say.
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		// Some providers report a cached prefix; it is counted separately so
		// the benchmark's token figures stay honest about what was charged.
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	// Error is either an object with a message or a bare string, depending on
	// the provider. HuggingFace returns the string form, and a client that
	// expects only the object reports "a body that is not JSON" while the
	// provider is plainly saying something useful -- in that case, that the
	// account had run out of credit.
	Error json.RawMessage `json:"error"`
}

// errorMessage reads whichever shape the provider used.
func (o oaResponse) errorMessage() string {
	if len(o.Error) == 0 {
		return ""
	}
	var asObject struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(o.Error, &asObject) == nil && asObject.Message != "" {
		return asObject.Message
	}
	var asString string
	if json.Unmarshal(o.Error, &asString) == nil && asString != "" {
		return asString
	}
	return strings.TrimSpace(string(o.Error))
}

// maxRateLimitWait bounds how long one call will wait out a rate limit in
// total, and maxRateLimitRetries is a backstop against a provider that asks for
// a trivial wait forever.
//
// Free and low tiers meter by tokens per minute, and the benchmark is exactly
// the workload that trips them: forty calls carrying a page each. Without this
// a run against such a tier fails most of its questions and reports a
// comparison drawn from the handful that got through, which is not a
// measurement of anything. The provider says how long to wait; waiting is the
// whole fix.
//
// The bound is cumulative time rather than a count of attempts, because a
// tokens-per-minute limit is not cleared by trying again -- it is cleared by
// the minute elapsing. Four attempts at the three seconds Groq asks for is
// twelve seconds of patience against a window that can need sixty, so the
// count ran out while the limit was still in force and eleven of forty
// questions were dropped from a run that was otherwise fine. Ninety seconds
// covers a full window with room to spare, and still ends a run against a
// provider that is genuinely too small for the workload.
const (
	maxRateLimitWait    = 90 * time.Second
	maxRateLimitRetries = 30
)

// retryAfter reads the delay a provider asked for, from the standard header or
// from the "try again in 19.2375s" that several of them put in the message.
func retryAfter(h http.Header, msg string) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && secs >= 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	if m := tryAgainRe.FindStringSubmatch(msg); m != nil {
		if secs, err := strconv.ParseFloat(m[1], 64); err == nil && secs >= 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return 0
}

var tryAgainRe = regexp.MustCompile(`try again in ([0-9.]+)\s*s`)

func (o *openAIClient) ask(ctx context.Context, model string, maxTokens int64,
	system string, blocks []ContentBlock) (*Result, error) {

	var waited time.Duration
	for attempt := 0; ; attempt++ {
		res, wait, err := o.askOnce(ctx, model, maxTokens, system, blocks)
		if wait <= 0 || attempt >= maxRateLimitRetries || waited+wait > maxRateLimitWait {
			if err != nil && wait > 0 {
				return nil, fmt.Errorf("%w (gave up after waiting %s across %d rate limits)",
					err, waited.Round(time.Second), attempt)
			}
			return res, err
		}
		// A cap on any single wait, because a provider asking for several
		// minutes at once is telling us this workload does not fit its tier,
		// and a benchmark that hangs is less useful than one that says so.
		if wait > 30*time.Second {
			return nil, fmt.Errorf("%w (asked to wait %s, which is longer than this is worth)",
				err, wait.Round(time.Second))
		}
		waited += wait
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait + 250*time.Millisecond):
		}
	}
}

// askOnce performs one attempt. A non-zero wait means the provider rate
// limited us and said when to come back.
func (o *openAIClient) askOnce(ctx context.Context, model string, maxTokens int64,
	system string, blocks []ContentBlock) (*Result, time.Duration, error) {

	msgs := make([]oaMessage, 0, 2)
	if system != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: system})
	}

	// A turn of nothing but text goes as a plain string, because that is what
	// every compatible provider accepts. Only a turn carrying an image needs
	// the parts array, and then the image travels as a data URL.
	hasImage := false
	for _, b := range blocks {
		if b.IsImage() {
			hasImage = true
			break
		}
	}
	if hasImage {
		parts := make([]oaPart, 0, len(blocks))
		for _, b := range blocks {
			if b.IsImage() {
				parts = append(parts, oaPart{
					Type:     "image_url",
					ImageURL: &oaImgURL{URL: "data:" + b.MediaType + ";base64," + b.B64},
				})
				continue
			}
			parts = append(parts, oaPart{Type: "text", Text: b.Text})
		}
		msgs = append(msgs, oaMessage{Role: "user", Content: parts})
	} else {
		var sb strings.Builder
		for i, b := range blocks {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
		}
		msgs = append(msgs, oaMessage{Role: "user", Content: sb.String()})
	}

	body, err := json.Marshal(oaRequest{Model: model, Messages: msgs, MaxTokens: maxTokens})
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}

	start := time.Now()
	resp, err := o.http.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return nil, 0, fmt.Errorf("call %s: %w", o.base, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, 0, err
	}

	var out oaResponse
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		return nil, 0, fmt.Errorf("%s returned %d and a body that is not JSON: %s",
			o.base, resp.StatusCode, truncateForError(string(raw)))
	}
	switch {
	case resp.StatusCode == http.StatusPaymentRequired:
		// Worth its own case: an exhausted account looks like a broken
		// integration until someone reads the body.
		return nil, 0, fmt.Errorf("%s refused the request for payment reasons: %s",
			o.base, messageOf(out, raw))
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, 0, fmt.Errorf("%w (the provider at %s rejected the key)", ErrNoCredentials, o.base)
	case resp.StatusCode == http.StatusTooManyRequests:
		msg := messageOf(out, raw)
		return nil, retryAfter(resp.Header, msg),
			fmt.Errorf("rate limited by %s: %s", o.base, msg)
	case resp.StatusCode >= 400:
		return nil, 0, fmt.Errorf("%s returned %d: %s", o.base, resp.StatusCode, messageOf(out, raw))
	case len(out.Choices) == 0:
		return nil, 0, fmt.Errorf("%s returned no choices: %s", o.base, truncateForError(string(raw)))
	}

	ch := out.Choices[0]
	res := &Result{
		Latency:    elapsed,
		StopReason: ch.FinishReason,
		Usage: Usage{
			InputTokens:     out.Usage.PromptTokens - out.Usage.PromptTokensDetails.CachedTokens,
			OutputTokens:    out.Usage.CompletionTokens,
			CacheReadTokens: out.Usage.PromptTokensDetails.CachedTokens,
			Calls:           1,
		},
	}
	if res.Usage.InputTokens < 0 {
		res.Usage.InputTokens = out.Usage.PromptTokens
		res.Usage.CacheReadTokens = 0
	}

	// A refusal arrives as a populated refusal field with empty content, which
	// is a successful response and must be reported as one rather than read as
	// an empty answer.
	if strings.TrimSpace(ch.Message.Refusal) != "" {
		res.Refused = true
		res.RefusalCategory = "refusal"
		return res, 0, nil
	}

	res.Text = strings.TrimSpace(ch.Message.Content)

	// A thinking model that never reached its answer.
	//
	// Reasoning models return the answer in content and their working in a
	// separate field. When the token ceiling is hit mid-thought, content comes
	// back empty and the working is all there is -- and an empty answer handed
	// to a grader is marked wrong, which is the same error as counting a failed
	// call as a wrong answer: the model never answered, so there is nothing to
	// mark. It is reported as a failure with the remedy in it instead.
	//
	// The working is deliberately not used as the answer. It is the model
	// talking to itself, frequently contradicts its own conclusion, and passing
	// it off as a reply would put words in its mouth.
	if res.Text == "" && !res.Refused {
		if thinking := ch.Message.Reasoning + ch.Message.ReasoningContent; thinking != "" {
			return res, 0, fmt.Errorf(
				"%s answered with reasoning but no content: it ran out of output tokens "+
					"before reaching an answer. Raise max tokens, or use a model that "+
					"does not think before replying", model)
		}
	}
	return res, 0, nil
}

func messageOf(out oaResponse, raw []byte) string {
	if m := out.errorMessage(); m != "" {
		return m
	}
	return truncateForError(string(raw))
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}
