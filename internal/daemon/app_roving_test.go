package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

func TestNavigatorAppRovingNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for roving fixtures in CI")
		}
		t.Skip("node not installed; roving fixtures NOT RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/app_roving_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("roving fixtures failed: %v\n%s", err, out)
	}
	for _, want := range []string{"tests 7", "pass 7", "fail 0", "cancelled 0", "skipped 0"} {
		if !regexp.MustCompile(`(?m)^# ` + want + `$`).Match(out) {
			t.Fatalf("roving fixtures incomplete:\n%s", out)
		}
	}
}
