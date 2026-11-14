package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

func TestNavigatorSessionControllerNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for controller fixtures in CI")
		}
		t.Skip("node is not installed; controller fixtures did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/session_controller_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("controller fixtures failed: %v\n%s", err, out)
	}
	for _, expected := range []string{"tests 23", "pass 23", "fail 0", "cancelled 0", "skipped 0"} {
		if !regexp.MustCompile(`(?m)^# ` + expected + `$`).Match(out) {
			t.Fatalf("controller fixtures did not complete all literal cases:\n%s", out)
		}
	}
}
