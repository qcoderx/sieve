package render

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestBrowserRegistryStartsFromAStableProcess guards the lifecycle boundary
// that used to race: registration must receive an already-started os.Process,
// never poll exec.Cmd.Process while Start is writing it.
func TestBrowserRegistryStartsFromAStableProcess(t *testing.T) {
	if os.Getenv("SIEVE_REAPER_HELPER") == "1" {
		for {
			time.Sleep(time.Second)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestBrowserRegistryStartsFromAStableProcess")
	cmd.Env = append(os.Environ(), "SIEVE_REAPER_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	trackBrowser(cmd.Process)
	browsers.Lock()
	got := browsers.live[cmd.Process.Pid]
	browsers.Unlock()
	if got != cmd.Process {
		t.Fatal("started process was not registered")
	}

	if killed := KillBrowsers(); killed != 1 {
		t.Fatalf("KillBrowsers killed %d processes, want 1", killed)
	}
}
