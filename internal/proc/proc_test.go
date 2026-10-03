package proc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestHelperSleep is the process TestEnvironReadsAnotherProcess inspects:
// the test binary itself, since macOS hides the environment of system
// binaries such as /bin/sleep.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("AIQ_PROC_HELPER") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(5 * time.Second)
}

func TestEnvironReadsAnotherProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSleep$")
	cmd.Env = append(os.Environ(), "AIQ_PROC_HELPER=1", "AIQ_LONG=1", "AIQ_LEASE=42")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	time.Sleep(200 * time.Millisecond)
	env, err := Environ(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if env["AIQ_LONG"] != "1" || env["AIQ_LEASE"] != "42" {
		t.Fatalf("AIQ_LONG=%q AIQ_LEASE=%q", env["AIQ_LONG"], env["AIQ_LEASE"])
	}
}
