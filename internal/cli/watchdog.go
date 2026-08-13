package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/qcoderx/sieve/internal/render"
)

// The watchdog is the promise that sieve always ends.
//
// Every stage already carries a deadline, and every deadline is honoured by the
// code that knows about it. That is not the same as the command ending: a
// context bounds the work that consults it, and a call that never returns --
// a CDP round trip into a browser that has stopped answering, a launch that
// never completes -- consults nothing. One pear.no run reported a total of
// twenty minutes and thirty-two seconds against a thirty-eight second budget.
//
// Every individual case of that is a bug and gets fixed as one. This exists
// because the class does not close: the failure is always in the call that was
// not thought about, and an agent driving sieve in a loop cannot be left
// waiting on the one that comes next.
//
// It is deliberately not a graceful shutdown. Whatever is stuck is, by
// definition, not responding to being asked nicely; a shutdown path that needs
// the stuck component to cooperate is the same bet again. It prints where the
// process was, so the next report has something in it, and exits.
type watchdog struct {
	stage atomic.Pointer[string]
	stop  chan struct{}
}

// watchdogGrace is how far past its own deadline a run may go before the
// process is killed.
//
// Generous on purpose. Teardown, writing an artifact and releasing a browser
// all happen after the deadline and are legitimate; the watchdog is not a
// second deadline and must never fire on a run that is merely finishing up.
// It fires on one that has stopped.
const watchdogGrace = 45 * time.Second

// exitHung is the code a caller can test for. It is deliberately distinct from
// a failed run (1), a usage error (2), a refusal by policy (3) and an
// unreachable host (4): "sieve hung" needs a different response from all four.
const exitHung = 5

// exitFunc is os.Exit, replaced in tests so firing can be observed without
// ending the test binary.
var exitFunc = defaultExit

func defaultExit(code int) { os.Exit(code) }

// startWatchdog arms the timer. The returned stop function disarms it.
func startWatchdog(w io.Writer, budget time.Duration, what string) (*watchdog, func()) {
	if budget <= 0 {
		return nil, func() {}
	}
	d := &watchdog{stop: make(chan struct{})}
	d.set("starting")

	limit := budget + watchdogGrace
	go func() {
		t := time.NewTimer(limit)
		defer t.Stop()
		select {
		case <-d.stop:
			return
		case <-t.C:
			d.fire(w, limit, what)
		}
	}()
	return d, func() { close(d.stop) }
}

func (d *watchdog) set(stage string) {
	if d == nil {
		return
	}
	d.stage.Store(&stage)
}

func (d *watchdog) fire(w io.Writer, limit time.Duration, what string) {
	stage := "unknown"
	if p := d.stage.Load(); p != nil {
		stage = *p
	}
	fmt.Fprintf(w, "\nsieve: giving up on %s after %v.\n", what, limit.Round(time.Second))
	fmt.Fprintf(w, "  The run passed its own deadline and then stopped responding to it,\n")
	fmt.Fprintf(w, "  which means something is blocked in a call that does not watch the\n")
	fmt.Fprintf(w, "  clock. Last stage reached: %s.\n", stage)
	fmt.Fprintf(w, "  This is a bug in sieve, not in the page. Please report it with the\n")
	fmt.Fprintf(w, "  URL and the stage above.\n\n")

	// The stacks are the whole point of reporting it: they name the call that
	// is stuck, which is the one piece of information the next fix needs.
	if os.Getenv("SIEVE_WATCHDOG_STACKS") != "" {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(w, "%s\n", buf[:n])
	} else {
		fmt.Fprintf(w, "  Re-run with SIEVE_WATCHDOG_STACKS=1 to include goroutine stacks.\n\n")
	}

	// Take the browser with us.
	//
	// os.Exit runs no deferred cleanup, so the Chromium tree this process
	// launched outlives it -- and a caller waiting on our process group is
	// still waiting. A sweep recorded a run that gave up at 83 seconds and did
	// not return to its caller for 2,167, because exiting alone is not exiting.
	if n := render.KillBrowsers(); n > 0 {
		fmt.Fprintf(w, "  Killed %d browser process tree(s) on the way out.\n\n", n)
	}

	// Exit code 5, because 3 and 4 are taken -- 3 is a refusal by safety policy
	// and 4 is an unreachable host. Using 3 here made a robots.txt refusal
	// indistinguishable from a hang in the first corpus pass that tried it.
	exitFunc(exitHung)
}
