package daemon

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNavigatorFeedNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for feed fixtures in CI")
		}
		t.Skip("node is not installed; feed fixtures did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/feed_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("feed fixtures failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "# pass 22") || !strings.Contains(string(out), "# fail 0") {
		t.Fatalf("feed fixtures did not complete all literal cases:\n%s", out)
	}
}

func TestNavigatorFeedNoParsingSinks(t *testing.T) {
	for _, file := range []string{"ui/feed.js", "ui/feed_data.js"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, pattern := range []string{
			`\b(?:innerHTML|outerHTML|insertAdjacentHTML|createContextualFragment|DOMParser|parseHTMLUnsafe|setHTMLUnsafe)\b`,
			`\b(?:eval|Function|fetch|XMLHttpRequest|WebSocket)\s*\(`,
			`\.\s*(?:style|cssText|href|src|srcdoc)\s*(?:=|\[|\.)`,
			`(?:document\.)?write(?:ln)?\s*\(`,
			`createElement\s*\(\s*["'](?:script|style|iframe|object|embed)["']`,
		} {
			if regexp.MustCompile(pattern).Match(source) {
				t.Errorf("%s acquired prohibited sink %s", file, pattern)
			}
		}
	}
}
