// Command sieve makes a heavy website readable by an agent.
//
// It takes the URL of a site built for human eyes -- WebGL heroes,
// scroll-driven reveals, pinned sections, text shattered into per-character
// spans -- and republishes it as a structured artifact that costs a fraction of
// the tokens and time to parse.
//
// It escalates rather than always rendering: most pages are answered by a plain
// HTTP fetch in well under a second, and the browser is used only where a cheap
// fetch comes back thin. Every artifact records which tier answered and why.
package main

import (
	"os"

	"github.com/qcoderx/sieve/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
