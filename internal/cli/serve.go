package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/qcoderx/sieve/internal/serve"
	"github.com/qcoderx/sieve/internal/render"
)

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		addr     string
		redirect bool
	)
	fs.StringVar(&addr, "addr", ":8080", "address to listen on")
	fs.BoolVar(&redirect, "redirect-humans", true,
		"send browsers to the original URL rather than the artifact")

	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: sieve serve <artifact-dir> [flags]

Serves distilled artifacts with content negotiation.

A request that looks like an agent -- an Accept header asking for markdown or
JSON, or a known agent user agent -- receives the distilled content. Everything
else is sent to the original URL, because a human should read the real site.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, perr := parseArgs(fs, args)
	if perr != nil {
		return 2
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}
	root := positional[0]

	h, err := serve.New(serve.Options{
		Root:           root,
		RedirectHumans: redirect,
	})
	if err != nil {
		return fail(stderr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	abs, _ := filepath.Abs(root)
	fmt.Fprintf(stdout, "sieve %s — serving %s on http://%s/\n", render.Version, abs, addr)
	for _, name := range h.Artifacts() {
		fmt.Fprintf(stdout, "  /%s\n", strings.TrimPrefix(name, "/"))
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fail(stderr, err)
	}
	return 0
}
