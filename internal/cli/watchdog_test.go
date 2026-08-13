package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestWatchdogDisarmsOnANormalRun: the common case is that it never fires, and
// a watchdog that fires on healthy runs is worse than none.
func TestWatchdogDisarmsOnANormalRun(t *testing.T) {
	var b bytes.Buffer
	dog, disarm := startWatchdog(&b, 50*time.Millisecond, "https://example.com")
	dog.set("fetch")
	disarm()

	// Well past budget+grace would be, if grace were not the point.
	time.Sleep(150 * time.Millisecond)
	if b.Len() != 0 {
		t.Errorf("a disarmed watchdog wrote output: %q", b.String())
	}
}

// TestWatchdogWaitsOutTheGrace covers the other half: teardown, writing an
// artifact and releasing a browser all legitimately happen after the deadline.
// The watchdog is not a second deadline, and must not fire on a run that is
// merely finishing up.
func TestWatchdogWaitsOutTheGrace(t *testing.T) {
	if watchdogGrace < 10*time.Second {
		t.Errorf("grace is %v: too tight to cover teardown after the deadline",
			watchdogGrace)
	}

	var b bytes.Buffer
	dog, disarm := startWatchdog(&b, time.Millisecond, "https://example.com")
	defer disarm()
	dog.set("closing the browser")

	// Comfortably past the budget, nowhere near budget+grace.
	time.Sleep(200 * time.Millisecond)
	if b.Len() != 0 {
		t.Errorf("the watchdog fired while the run was still within its grace: %q", b.String())
	}
}

// TestWatchdogReportsWhereItStopped: the stage is the one piece of information
// the next fix needs, and it is why this prints rather than only exiting.
//
// fire is called directly. Letting the timer call it would end the test binary,
// since firing exits the process on purpose -- whatever is stuck is by
// definition not responding to being asked nicely, and a shutdown path that
// needs the stuck component to cooperate is the same bet that failed.
func TestWatchdogReportsWhereItStopped(t *testing.T) {
	d := &watchdog{stop: make(chan struct{})}
	d.set("sweep")

	var b bytes.Buffer
	func() {
		defer func() { _ = recover() }() // fire ends with os.Exit; guard anyway
		exitFunc = func(int) { panic("exit") }
		defer func() { exitFunc = defaultExit }()
		d.fire(&b, 80*time.Second, "https://pear.no")
	}()

	out := b.String()
	for _, want := range []string{"pear.no", "sweep", "1m20s"} {
		if !strings.Contains(out, want) {
			t.Errorf("watchdog report missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "bug in sieve") {
		t.Error("the report does not say whose fault this is; a user will blame the page")
	}
}
