package render

import (
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// The browser registry exists for one case: the process is about to die without
// running its deferred cleanup.
//
// chromedp kills its browser when the allocator context is cancelled, and every
// ordinary path cancels it. os.Exit does not: it runs no defers, so a watchdog
// firing leaves the whole Chromium tree behind. That is not a tidiness problem.
// A caller that redirected our output is waiting on the process group, so the
// orphans keep the command open long after we are gone -- one sweep recorded a
// run that gave up at 83 seconds and did not return to its caller for 2,167.
//
// Exiting without taking the browser with us is not exiting, from the point of
// view of the only party that cares.
var browsers struct {
	sync.Mutex
	live map[int]*exec.Cmd
}

func init() { browsers.live = map[int]*exec.Cmd{} }

// trackBrowser records a launched browser so it can be killed without its
// context. It is registered through chromedp's ModifyCmdFunc, which runs after
// the command is built and before it starts.
func trackBrowser(cmd *exec.Cmd) {
	go func() {
		// The pid does not exist until Start, and ModifyCmdFunc runs before it.
		// Poll briefly rather than reaching into chromedp's lifecycle.
		for i := 0; i < 100; i++ {
			if cmd.Process != nil {
				browsers.Lock()
				browsers.live[cmd.Process.Pid] = cmd
				browsers.Unlock()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// forgetBrowser drops a browser that shut down the ordinary way.
func forgetBrowser(pid int) {
	browsers.Lock()
	delete(browsers.live, pid)
	browsers.Unlock()
}

// KillBrowsers terminates every browser this process launched, and their
// children, without waiting for anything to agree.
//
// Called from the watchdog immediately before the process exits. It is
// deliberately violent: whatever state the browser is in, we have already
// concluded it is not answering, and asking politely is the bet that just lost.
// It reports how many it killed so the watchdog can say so.
func KillBrowsers() int {
	browsers.Lock()
	cmds := make([]*exec.Cmd, 0, len(browsers.live))
	for _, c := range browsers.live {
		cmds = append(cmds, c)
	}
	browsers.live = map[int]*exec.Cmd{}
	browsers.Unlock()

	n := 0
	for _, c := range cmds {
		if c.Process == nil {
			continue
		}
		// Skip one that has already gone. chromedp waits on the command itself,
		// so a browser closed the ordinary way has its state set here, and
		// signalling a finished pid risks hitting whatever inherited the number.
		if c.ProcessState != nil {
			continue
		}
		pid := c.Process.Pid
		// Chromium is a tree: the browser process spawns renderers, a GPU
		// process and utilities, and killing only the parent leaves the rest
		// holding the handles that keep a caller waiting.
		if runtime.GOOS == "windows" {
			// /T takes the tree, /F does not ask.
			_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
		} else {
			// Negative pid addresses the process group, which chromedp's
			// allocator sets up for exactly this reason.
			_ = syscallKillGroup(pid)
		}
		_ = c.Process.Kill()
		n++
	}
	return n
}
