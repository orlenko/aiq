package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNavigatorAsksMovedViewNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for Asks moved view fixtures in CI")
		}
		t.Skip("node is not installed; Asks moved view fixtures did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/asks_moved_view_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Asks moved view fixtures failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "# pass 16") || !strings.Contains(string(out), "# fail 0") {
		t.Fatalf("Asks moved view fixtures did not complete all literal cases:\n%s", out)
	}
}
