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
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// maxRateLimitRetries bounds how many times a call waits out a rate limit.
//
// Free and low tiers meter by tokens per minute, and the benchmark is exactly
// the workload that trips them: forty calls carrying a page each. Without this
// a run against such a tier fails most of its questions and reports a
// comparison drawn from the handful that got through, which is not a
// measurement of anything. The provider says how long to wait; waiting is the
// whole fix.
const maxRateLimitRetries = 4

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

	var last error
	for attempt := 0; ; attempt++ {
		res, wait, err := o.askOnce(ctx, model, maxTokens, system, blocks)
		if wait <= 0 || attempt >= maxRateLimitRetries {
			if err != nil && wait > 0 {
				return nil, fmt.Errorf("%w (gave up after %d rate-limit waits)",
					err, maxRateLimitRetries)
			}
			return res, err
		}
		last = err
		// A cap, because a provider asking for several minutes is telling us
		// this workload does not fit its tier, and a benchmark that hangs is
		// less useful than one that says so.
		if wait > 30*time.Second {
			return nil, fmt.Errorf("%w (asked to wait %s, which is longer than this is worth)",
				last, wait.Round(time.Second))
		}
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
	return res, 0, nil
}

func messageOf(out oaResponse, raw []byte) string {
	if out.Error != nil && out.Error.Message != "" {
		return out.Error.Message
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
