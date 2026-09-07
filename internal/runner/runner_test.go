package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// A worker gets aiq's stdin verbatim (orchestrators pipe the prompt in), and
// a usage-limit rejection on stderr is recognised even though stdout is
// ignored by the caller.
func TestRunWorkerPassesStdinAndDetectsLimit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "child.sh")
	got := filepath.Join(dir, "stdin.txt")
	os.WriteFile(script, []byte("#!/bin/sh\ncat > "+got+"\necho 'ERROR: You have hit your usage limit. Try again at 10:23.' >&2\nexit 1\n"), 0o755)

	r, w, _ := os.Pipe()
	w.WriteString("persona prompt, several KB in real life\n")
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	res, err := RunWorker(exec.Command(script), []*regexp.Regexp{regexp.MustCompile(`(?i)hit your (usage|session|weekly) limit`)})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(got)
	if string(data) != "persona prompt, several KB in real life\n" {
		t.Fatalf("stdin not forwarded: %q", data)
	}
	if res.Code != 1 || !res.LimitHit || res.Total > 1024 {
		t.Fatalf("%+v", res)
	}
}
