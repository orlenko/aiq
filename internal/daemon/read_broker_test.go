package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

func TestNavigatorReadBrokerNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for broker fixtures in CI")
		}
		t.Skip("node not installed; broker fixtures NOT RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/read_broker_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("broker fixtures failed: %v\n%s", err, out)
	}
	for _, want := range []string{"tests 8", "pass 8", "fail 0", "cancelled 0", "skipped 0"} {
		if !regexp.MustCompile(`(?m)^# ` + want + `$`).Match(out) {
			t.Fatalf("broker fixtures incomplete:\n%s", out)
		}
	}
}
