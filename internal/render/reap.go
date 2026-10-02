package render

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
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
	live map[int]*os.Process
}

func init() { browsers.live = map[int]*os.Process{} }

// trackBrowser records a launched browser so it can be killed without its
// context. Launch calls this only after chromedp.Run has returned from starting
// Chromium, so Process and its PID are already immutable. The old command hook
// polled exec.Cmd.Process while os/exec.Start was assigning it, which was a real
// data race and also replaced chromedp's Linux parent-death configuration.
func trackBrowser(process *os.Process) {
	if process == nil {
		return
	}
	browsers.Lock()
	browsers.live[process.Pid] = process
	browsers.Unlock()
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
	processes := make([]*os.Process, 0, len(browsers.live))
	for _, process := range browsers.live {
		processes = append(processes, process)
	}
	browsers.live = map[int]*os.Process{}
	browsers.Unlock()

	n := 0
	for _, process := range processes {
		pid := process.Pid
		killed := false
		// Chromium is a tree: the browser process spawns renderers, a GPU
		// process and utilities, and killing only the parent leaves the rest
		// holding the handles that keep a caller waiting.
		if runtime.GOOS == "windows" {
			// /T takes the tree, /F does not ask.
			killed = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run() == nil
		} else {
			// Try the process group first in case Chromium is its leader; the
			// direct process kill below is the portable fallback.
			killed = syscallKillGroup(pid) == nil
		}
		if err := process.Kill(); err == nil {
			killed = true
		}
		if killed {
			n++
		}
	}
	return n
}
