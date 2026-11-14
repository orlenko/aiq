package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

func TestNavigatorTabProofNodeFixtures(t *testing.T) {
	node, e := exec.LookPath("node")
	if e != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node required for tab proof fixtures")
		}
		t.Skip("node absent; tab proof fixtures NOT RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/tab_proof_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("tab proof fixtures failed: %v\n%s", e, out)
	}
	for _, s := range []string{"tests 8", "pass 8", "fail 0", "cancelled 0", "skipped 0"} {
		if !regexp.MustCompile(`(?m)^# ` + s + `$`).Match(out) {
			t.Fatalf("tab proof fixtures incomplete:\n%s", out)
		}
	}
}
