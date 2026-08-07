package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/escalate"
	"github.com/qcoderx/sieve/internal/mcpserver"
	"github.com/qcoderx/sieve/internal/safety"
)

// connect wires a client to the server over an in-memory transport, so the test
// exercises the real protocol surface rather than calling handlers directly.
func connect(t *testing.T, fixtures *httptest.Server) (*mcp.ClientSession, func()) {
	t.Helper()

	dopts := distill.DefaultOptions()
	// The fixture server is on loopback, which the guard refuses by default --
	// correctly, since that is the SSRF case. Tests are the one place it is
	// appropriate to allow.
	cfg := safety.DefaultGuardConfig()
	cfg.AllowPrivate = true
	dopts.Guard = safety.NewGuard(cfg)
	dopts.MaxTier = escalate.TierFetch // no browser needed for these assertions
	dopts.Memory = escalate.NewMemory()

	srv := mcpserver.New(mcpserver.Options{Distill: dopts})
	m := srv.MCPServer()

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = m.Run(ctx, serverT)
	}()

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := c.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return sess, func() {
		_ = sess.Close()
		srv.Close()
		<-serverDone
	}
}

func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.FileServer(http.Dir("../../testdata/pages")))
	t.Cleanup(s.Close)
	return s
}

func callJSON(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error: %+v", name, res.Content)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: marshal result: %v", name, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: unmarshal result: %v\n%s", name, err, b)
	}
}

func TestToolSurface(t *testing.T) {
	fx := fixtureServer(t)
	sess, done := connect(t, fx)
	defer done()

	ctx := context.Background()
	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	found := map[string]bool{}
	for _, tl := range tools.Tools {
		found[tl.Name] = true
		if tl.Description == "" {
			t.Errorf("tool %q has no description; the description is the signal a model uses "+
				"to route a call, not documentation for humans", tl.Name)
		}
	}
	for _, want := range []string{
		"distill", "status", "get_content", "search_content",
		"list_actions", "get_hidden_content", "describe_media",
	} {
		if !found[want] {
			t.Errorf("tool %q is missing", want)
		}
	}

	// Hidden content must be its own tool, never a flag on the content call.
	// A flag is one typo away from being set by default.
	for _, tl := range tools.Tools {
		if tl.Name != "get_content" {
			continue
		}
		schema, _ := json.Marshal(tl.InputSchema)
		low := strings.ToLower(string(schema))
		for _, forbidden := range []string{"hidden", "latent", "include_all"} {
			if strings.Contains(low, forbidden) {
				t.Errorf("get_content exposes a %q parameter; hidden content must only be "+
					"reachable through its own tool", forbidden)
			}
		}
	}
}

func TestDistillReturnsManifestNotBody(t *testing.T) {
	fx := fixtureServer(t)
	sess, done := connect(t, fx)
	defer done()

	var out struct {
		JobID    string `json:"job_id"`
		State    string `json:"state"`
		Manifest *struct {
			Title    string `json:"title"`
			Sections []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				Tokens int    `json:"est_tokens"`
			} `json:"sections"`
			Counts struct {
				Blocks      int `json:"blocks"`
				Latent      int `json:"latent"`
				TotalTokens int `json:"est_total_tokens"`
			} `json:"counts"`
			Guidance string `json:"guidance"`
		} `json:"manifest"`
	}
	callJSON(t, sess, "distill", map[string]any{
		"url": fx.URL + "/adversarial/", "wait_seconds": 60,
	}, &out)

	if out.State != "ready" {
		t.Fatalf("state = %q, want ready", out.State)
	}
	if out.Manifest == nil {
		t.Fatal("distill returned no manifest")
	}
	if out.Manifest.Title == "" {
		t.Error("manifest has no title")
	}
	if len(out.Manifest.Sections) == 0 {
		t.Error("manifest lists no sections; an agent has nothing to choose from")
	}
	if out.Manifest.Counts.TotalTokens <= 0 {
		t.Error("manifest does not say what the whole artifact would cost, " +
			"which is exactly the number an agent needs in order to decide not to fetch it")
	}
	if out.Manifest.Guidance == "" {
		t.Error("manifest carries no guidance; tool results are read by a model, not a human")
	}

	// The whole point: distill must not return the page body.
	raw, _ := json.Marshal(out.Manifest)
	for _, bodyText := range []string{
		"wheel-thrown and wood-fired",
		"anagama kiln is fired twice a year",
	} {
		if strings.Contains(string(raw), bodyText) {
			t.Errorf("distill returned page body text (%q). Returning the artifact would move "+
				"the token cost rather than remove it, which defeats the whole premise.", bodyText)
		}
	}

	// --- the content tools ---------------------------------------------
	jobID := out.JobID

	var search struct {
		Hits []struct {
			BlockID string  `json:"block_id"`
			Snippet string  `json:"snippet"`
			Score   float64 `json:"score"`
		} `json:"hits"`
		Notice string `json:"notice"`
	}
	callJSON(t, sess, "search_content", map[string]any{
		"job_id": jobID, "query": "kiln firing",
	}, &search)
	if len(search.Hits) == 0 {
		t.Error("search found nothing for a query that appears in the page")
	}
	if search.Notice == "" {
		t.Error("search result carries no untrusted-data notice")
	}

	var content struct {
		Blocks []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"blocks"`
		Notice string `json:"notice"`
	}
	callJSON(t, sess, "get_content", map[string]any{"job_id": jobID}, &content)
	if len(content.Blocks) == 0 {
		t.Fatal("get_content returned nothing")
	}

	// Every channel the visibility defence closes must be absent here.
	joined := ""
	for _, b := range content.Blocks {
		joined += b.Text + "\n"
	}
	for _, injected := range []string{
		"INJECT_HIDDEN_TAB", "INJECT_CONTRAST", "INJECT_OPACITY",
		"INJECT_JSONLD_UNWHITELISTED",
	} {
		if strings.Contains(joined, injected) {
			t.Errorf("get_content leaked %s into a content response", injected)
		}
	}

	// --- the hidden-content tool ----------------------------------------
	var hidden struct {
		Blocks []struct {
			ID           string `json:"id"`
			Text         string `json:"text"`
			Trust        string `json:"trust"`
			ControlLabel string `json:"control_label"`
		} `json:"blocks"`
		Warning string `json:"warning"`
	}
	callJSON(t, sess, "get_hidden_content", map[string]any{"job_id": jobID}, &hidden)

	if hidden.Warning == "" {
		t.Error("the hidden-content tool returned no warning in its payload; " +
			"the tool description is read once at registration, the payload every time")
	}
	if !strings.Contains(strings.ToLower(hidden.Warning), "never rendered") {
		t.Errorf("the warning does not say the content was hidden: %q", hidden.Warning)
	}
	for _, b := range hidden.Blocks {
		if b.Trust == "" {
			t.Errorf("latent block %s lost its trust marker in transit", b.ID)
		}
	}
}

func TestUnknownJobIsAClearError(t *testing.T) {
	fx := fixtureServer(t)
	sess, done := connect(t, fx)
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_content", Arguments: map[string]any{"job_id": "job_999"},
	})
	if err == nil && !res.IsError {
		t.Fatal("asking for an unknown job should be an error")
	}
}
