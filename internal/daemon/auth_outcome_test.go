package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

func TestNavigatorAuthOutcomeNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node required for auth outcome fixtures")
		}
		t.Skip("node not installed; auth outcome fixtures NOT RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/auth_outcome_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("auth outcome fixtures failed: %v\n%s", err, out)
	}
	for _, want := range []string{"tests 10", "pass 10", "fail 0", "cancelled 0", "skipped 0"} {
		if !regexp.MustCompile(`(?m)^# ` + want + `$`).Match(out) {
			t.Fatalf("auth outcome fixtures incomplete:\n%s", out)
		}
	}
}
