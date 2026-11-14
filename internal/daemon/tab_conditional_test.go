package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

func TestNavigatorConditionalTabNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node required for conditional proof fixtures")
		}
		t.Skip("node absent; conditional proof fixtures NOT RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/tab_conditional_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("conditional fixtures failed: %v\n%s", err, out)
	}
	for _, expected := range []string{"tests 7", "pass 7", "fail 0", "cancelled 0", "skipped 0"} {
		if !regexp.MustCompile(`(?m)^# ` + expected + `$`).Match(out) {
			t.Fatalf("conditional fixtures incomplete:\n%s", out)
		}
	}
}
