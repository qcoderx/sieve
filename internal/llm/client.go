// Package llm wraps a chat model for the two places sieve needs one:
// captioning canvas regions that yielded nothing to the scene-graph parse, and
// running the benchmark's question sets.
//
// It is deliberately thin. The value it adds over calling a provider's SDK
// directly is in the parts that are easy to get wrong and that both callers
// need: refusal handling, a per-job spend ceiling, and usage accounting exact
// enough to put in a benchmark table.
//
// # Two providers, one shape
//
// Anthropic is the default. Setting a base URL switches to the OpenAI chat
// completions shape, which is what almost every other provider speaks --
// Groq, OpenRouter, Together, vLLM, Ollama and the rest -- so a user can point
// sieve at whichever model they already pay for, or at a local one.
//
// That matters beyond convenience. The benchmark sends a page's full HTML with
// every question, which on the default model costs real money per site; being
// able to run it against a free or self-hosted model is the difference between
// a benchmark anyone can reproduce and one only its author ever runs.
package llm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is the model used unless the caller names another.
const DefaultModel = "claude-opus-5"

// ErrNoCredentials is returned when no API key is configured. It explains what
// to do rather than surfacing a 401 from three layers down.
var ErrNoCredentials = errors.New(
	"no model credentials found\n" +
		"  Anthropic: set ANTHROPIC_API_KEY, or run `ant auth login`.\n" +
		"  Any other provider: set LLM_BASE_URL, LLM_API_KEY and LLM_MODEL.\n" +
		"    Most providers expose an OpenAI-compatible endpoint, e.g.\n" +
		"    LLM_BASE_URL=https://api.groq.com/openai/v1\n" +
		"  Only the vision and benchmark paths need this; distillation does not.")

// ErrBudgetExhausted is returned when a job's token ceiling is reached. Vision
// is the most expensive step in the pipeline by an order of magnitude, so the
// ceiling is a hard stop rather than a warning.
var ErrBudgetExhausted = errors.New("model budget for this job is exhausted")

// Usage is what a call actually cost. These numbers come from the API's own
// accounting, not from an estimate, which is why the benchmark can report them
// as measurements.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	Calls            int   `json:"calls"`
}

// Add accumulates another call's usage.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.Calls += o.Calls
}

// Total is every input token the call was charged for, cached or not.
func (u Usage) Total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Options configures a client.
type Options struct {
	APIKey string
	Model  string
	// BaseURL selects an OpenAI-compatible provider. Empty means Anthropic.
	//
	// Resolved from LLM_BASE_URL when unset, so a user can switch provider
	// without every caller growing a flag.
	BaseURL string
	// MaxTokens bounds a single response.
	MaxTokens int64
	// Effort trades thoroughness against cost: low, medium, high, xhigh, max.
	// Empty leaves the API default.
	Effort string
	// Budget caps total tokens across the client's lifetime. Zero means no cap.
	Budget int64
	// Timeout bounds one request.
	Timeout time.Duration
}

// Client is a budgeted wrapper around one provider's chat endpoint.
type Client struct {
	api anthropic.Client
	// oai is set instead of api when a base URL selected the OpenAI shape.
	oai   *openAIClient
	opts  Options
	mu    sync.Mutex
	usage Usage
}

// New builds a client. The API key is resolved from the options, then from
// ANTHROPIC_API_KEY; when neither is set the SDK's own credential chain is left
// to find a stored profile, and only a call will reveal whether it succeeded.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		opts.BaseURL = strings.TrimSpace(os.Getenv("LLM_BASE_URL"))
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 2048
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}

	// An OpenAI-compatible provider, when one was named.
	if opts.BaseURL != "" {
		key := opts.APIKey
		if key == "" {
			key = firstEnv("LLM_API_KEY", "OPENAI_API_KEY")
		}
		if opts.Model == "" {
			opts.Model = strings.TrimSpace(os.Getenv("LLM_MODEL"))
		}
		if opts.Model == "" {
			return nil, fmt.Errorf("a base URL was given (%s) but no model name; "+
				"set LLM_MODEL or pass -model. There is no default because every "+
				"provider names its models differently", opts.BaseURL)
		}
		return &Client{oai: newOpenAI(opts.BaseURL, key, opts.Timeout), opts: opts}, nil
	}

	if opts.Model == "" {
		opts.Model = DefaultModel
	}

	var sdkOpts []option.RequestOption
	key := opts.APIKey
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	if key != "" {
		sdkOpts = append(sdkOpts, option.WithAPIKey(key))
	}
	sdkOpts = append(sdkOpts, option.WithRequestTimeout(opts.Timeout))

	return &Client{api: anthropic.NewClient(sdkOpts...), opts: opts}, nil
}

// firstEnv returns the first of these environment variables that is set.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// HasCredentials reports whether anything is configured to authenticate with.
// It is a pre-flight check so a long distillation does not run to completion
// and then fail on its last step.
func HasCredentials(explicit string) bool {
	return explicit != "" ||
		firstEnv("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
			"LLM_API_KEY", "OPENAI_API_KEY", "LLM_BASE_URL") != ""
}

// Usage reports the running total.
func (c *Client) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

// Model reports which model this client calls.
func (c *Client) Model() string { return c.opts.Model }

// Result is one completed call.
type Result struct {
	Text  string
	Usage Usage
	// Refused is true when the model's safety classifiers declined the request.
	// This is a successful HTTP response, not an error, and it has to be
	// checked before reading Text.
	Refused bool
	// RefusalCategory is the policy category, when the API supplies one.
	RefusalCategory string
	// Latency is wall clock for the call.
	Latency time.Duration
	// StopReason is the raw stop reason, for the benchmark report.
	StopReason string
}

// Ask sends a single-turn request and returns the text response.
//
// system may be empty. blocks is the user turn's content, which lets callers
// mix text and images without this package knowing anything about either.
func (c *Client) Ask(ctx context.Context, system string, blocks []ContentBlock) (*Result, error) {
	if err := c.reserve(); err != nil {
		return nil, err
	}

	if c.oai != nil {
		out, err := c.oai.ask(ctx, c.opts.Model, c.opts.MaxTokens, system, blocks)
		if err != nil {
			return nil, err
		}
		c.record(out.Usage)
		return out, nil
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.opts.Model),
		MaxTokens: c.opts.MaxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(toAnthropic(blocks)...)},
	}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}
	if c.opts.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(c.opts.Effort),
		}
	}

	start := time.Now()
	resp, err := c.api.Messages.New(ctx, params)
	elapsed := time.Since(start)
	if err != nil {
		return nil, translateErr(err)
	}

	out := &Result{
		Latency:    elapsed,
		StopReason: string(resp.StopReason),
		Usage: Usage{
			InputTokens:      resp.Usage.InputTokens,
			OutputTokens:     resp.Usage.OutputTokens,
			CacheReadTokens:  resp.Usage.CacheReadInputTokens,
			CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
			Calls:            1,
		},
	}
	c.record(out.Usage)

	// A refusal comes back as a normal 200 with an empty or partial content
	// array. Reading content[0] without checking this is the standard way to
	// turn a refusal into a nil dereference.
	if resp.StopReason == anthropic.StopReasonRefusal {
		out.Refused = true
		out.RefusalCategory = string(resp.StopDetails.Category)
		return out, nil
	}

	var sb strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	out.Text = strings.TrimSpace(sb.String())
	return out, nil
}

func (c *Client) reserve() error {
	if c.opts.Budget <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.usage.Total() >= c.opts.Budget {
		return fmt.Errorf("%w: %d tokens spent against a ceiling of %d",
			ErrBudgetExhausted, c.usage.Total(), c.opts.Budget)
	}
	return nil
}

func (c *Client) record(u Usage) {
	c.mu.Lock()
	c.usage.Add(u)
	c.mu.Unlock()
}

// translateErr turns the SDK's errors into ones a user can act on.
func translateErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 401:
			return fmt.Errorf("%w (the API rejected the credentials that were found)", ErrNoCredentials)
		case 429:
			return fmt.Errorf("rate limited by the Anthropic API: %w", err)
		}
	}
	return err
}

// ContentBlock is a piece of a user turn: either text or an image.
//
// It is deliberately this package's own type rather than a provider's. Callers
// build a request once and it goes to whichever provider is configured; making
// it an alias for one SDK's union would have put that SDK in the signature of
// every caller and made a second provider impossible to add without changing
// all of them.
type ContentBlock struct {
	Text string
	// MediaType and B64 are set instead of Text for an image.
	MediaType string
	B64       string
}

// IsImage reports whether this block carries an image.
func (b ContentBlock) IsImage() bool { return b.B64 != "" }

// TextBlock is a convenience for building a user turn.
func TextBlock(s string) ContentBlock {
	return ContentBlock{Text: s}
}

// ImageBlock builds an image content block from base64-encoded bytes.
func ImageBlock(mediaType string, b64 string) ContentBlock {
	return ContentBlock{MediaType: mediaType, B64: b64}
}

// toAnthropic converts neutral blocks into the Anthropic SDK's union type.
func toAnthropic(blocks []ContentBlock) []anthropic.ContentBlockParamUnion {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		if b.IsImage() {
			out = append(out, anthropic.NewImageBlockBase64(b.MediaType, b.B64))
			continue
		}
		out = append(out, anthropic.NewTextBlock(b.Text))
	}
	return out
}
