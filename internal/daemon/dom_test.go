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

func TestNavigatorDOMNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for DOM fixtures in CI")
		}
		t.Skip("node is not installed; DOM fixtures did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", "--test-reporter=tap", "ui/dom_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DOM fixtures failed: %v\n%s", err, out)
	}
	// A successful command that discovered no tests is not verification.
	if !strings.Contains(string(out), "# pass 10") || !strings.Contains(string(out), "# fail 0") {
		t.Fatalf("DOM fixtures did not complete all literal cases:\n%s", out)
	}
}

func TestNavigatorDOMNoParsingSinks(t *testing.T) {
	source, err := os.ReadFile("ui/dom.js")
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
			t.Errorf("DOM source acquired a prohibited parsing/execution/style sink: %s", pattern)
		}
	}
	// This is a narrow sink-regression check. Browser layout, CSP and Trusted
	// Types enforcement require the separate actual-browser fixture.
}
