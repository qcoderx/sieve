package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qcoderx/sieve/internal/canvas"
	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/mcpserver"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/safety"
)

func runMCP(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		common commonFlags
		addr   string
		vision bool
	)
	common.register(fs)
	fs.StringVar(&addr, "http", "",
		"serve Streamable HTTP on this address instead of stdio, e.g. :8080")
	fs.BoolVar(&vision, "vision", false,
		"allow a vision model to describe canvas regions (off by default)")

	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: sieve mcp [flags]

Runs the Model Context Protocol server. This is sieve's primary interface: the
CLI is for humans and CI, and the HTTP server exists mainly to host a shared
cache.

By default it speaks stdio, which is what a local host launches. Both transports
serve the same tool surface from the same core, so behaviour cannot drift
between them.

Register with Claude Code:
  claude mcp add sieve -- sieve mcp

Or a hosted instance:
  claude mcp add --transport http sieve https://sieve.example.com/mcp

Flags:
`)
		fs.PrintDefaults()
	}
	if _, perr := parseArgs(fs, args); perr != nil {
		return 2
	}

	w, h, err := common.viewportSize()
	if err != nil {
		return fail(stderr, err)
	}
	minTier, maxTier, err := common.tiers()
	if err != nil {
		return fail(stderr, err)
	}

	dopts := distill.DefaultOptions()
	dopts.MinTier, dopts.MaxTier = minTier, maxTier
	if dopts.MaxTier == "" {
		dopts.MaxTier = distill.DefaultOptions().MaxTier
	}
	// An agent calling distill means the URL is attacker-influenced in the
	// general case, so the guard is not optional here whatever the CLI allows.
	guardCfg := safety.DefaultGuardConfig()
	guardCfg.AllowPrivate = false
	dopts.Guard = safety.NewGuard(guardCfg)
	dopts.Limiter = common.limiter()
	dopts.Robots = safety.NewRobotsCache(nil)
	dopts.Memory = loadMemory(common.memoryPath)
	dopts.Render.ViewportW, dopts.Render.ViewportH = w, h
	dopts.Render.ChromePath = common.chrome
	// The timeouts the caller asked for, which this command was accepting and
	// then ignoring.
	//
	// common.register puts -timeout and -load-timeout on every subcommand, and
	// distill applies both. This one set the viewport and the browser path and
	// stopped, so the MCP server always ran on defaults however it was started.
	// The effect was not subtle: igloo.inc reads correctly from the CLI with a
	// longer load budget and returned an empty shell over MCP, which is the
	// interface almost every user will actually reach it through. A flag that
	// is advertised and silently discarded is worse than one that does not
	// exist.
	dopts.Render.ScaleTo(common.timeout)
	dopts.Render.LoadBudget = common.loadTimeout
	dopts.Canvas = canvas.DefaultOptions()
	dopts.Canvas.EnableVision = vision

	if common.verbose {
		dopts.Logf = func(format string, a ...any) {
			fmt.Fprintf(stderr, "  "+format+"\n", a...)
		}
	}

	srv := mcpserver.New(mcpserver.Options{Distill: dopts})
	defer srv.Close()
	defer saveMemory(common.memoryPath, dopts.Memory)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := srv.MCPServer()

	if addr == "" {
		// stdio. Nothing may be written to stdout except protocol frames, so
		// every diagnostic goes to stderr.
		fmt.Fprintf(stderr, "sieve %s — MCP server on stdio\n", render.Version)
		if err := m.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
			return fail(stderr, err)
		}
		return 0
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return m }, nil)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           http.TimeoutHandler(handler, 10*time.Minute, "sieve: request timed out"),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()

	fmt.Fprintf(stderr, "sieve %s — MCP server on http://%s/\n", render.Version, addr)
	fmt.Fprint(stderr, "Terminate TLS properly in front of this in production; some hosts require HTTPS.\n")
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fail(stderr, err)
	}
	return 0
}
