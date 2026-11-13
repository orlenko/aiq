package herdr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unsetContext(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentCoreUsesOwnerWithoutSelectorFallback(t *testing.T) {
	for _, owner := range []string{"", "relative.sock", "/invented/unavailable-owner.sock"} {
		t.Run(owner, func(t *testing.T) {
			selector, calls := addressEndpoint(t, 1, "selector-core", "", "live", os.Getpid())
			t.Setenv("HC_SOCKET", selector)
			t.Setenv("HC_TERMINAL_SOCKET", owner)
			t.Setenv("HC_TERMINAL_ID", "t1")
			t.Setenv("TMUX", "")
			t.Setenv("TMUX_PANE", "")
			t.Setenv("STY", "")
			if _, err := CurrentCore(); err == nil {
				t.Fatal("invalid owner accepted")
			}
			if got := calls(); len(got) != 0 {
				t.Fatalf("failed owner retried selector: %v", got)
			}
		})
	}
	for _, generation := range []int{1, 2} {
		t.Run(string(rune('0'+generation)), func(t *testing.T) {
			owner, ownerCalls := addressEndpoint(t, generation, "owner-core", "different-address", "live", os.Getpid())
			selector, selectorCalls := addressEndpoint(t, 1, "selector-core", "", "live", os.Getpid())
			t.Setenv("HC_SOCKET", selector)
			t.Setenv("HC_TERMINAL_SOCKET", owner)
			t.Setenv("HC_TERMINAL_ID", "t1")
			t.Setenv("TMUX", "")
			t.Setenv("TMUX_PANE", "")
			t.Setenv("STY", "")
			core, err := CurrentCore()
			if err == nil || core.Socket != owner || len(ownerCalls()) == 0 || len(selectorCalls()) != 0 {
				t.Fatalf("owner verification fell back: core=%+v err=%v ownerCalls=%v selectorCalls=%v", core, err, ownerCalls(), selectorCalls())
			}
		})
	}
}

func TestCurrentSocketDistinguishesOwnerPresence(t *testing.T) {
	t.Setenv("HC_SOCKET", "/invented/selected-core")
	unsetContext(t, "HC_TERMINAL_SOCKET")
	if got := currentSocket(); got != "/invented/selected-core" {
		t.Fatalf("explicit selector lost: %q", got)
	}
	for _, owner := range []string{"", "relative.sock", "/invented/owner-core"} {
		t.Setenv("HC_TERMINAL_SOCKET", owner)
		if got := currentSocket(); got != owner {
			t.Fatalf("owning endpoint replaced: %q", got)
		}
	}
}

func TestExternalAttachKeepsSavedEndpointAndClearsCallerContext(t *testing.T) {
	socket, _ := addressEndpoint(t, 1, "target-core", "t1", "live", os.Getpid())
	core := Core{Socket: socket, ID: "target-core", Terminal: "t1", PID: os.Getpid()}
	t.Setenv("HC_TERMINAL_ID", "t2")
	t.Setenv("HC_TERMINAL_SOCKET", "/invented/caller-owner")
	t.Setenv("HC_SOCKET", "/invented/caller-selector")
	t.Setenv("HC_SESSION", "invented-session")
	root := t.TempDir()
	marker := filepath.Join(root, "attach")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" \"$HC_SOCKET\" \"${HC_TERMINAL_SOCKET-unset}\" \"${HC_TERMINAL_ID-unset}\" \"${HC_SESSION-unset}\" > \"$GO_HERDR_ATTACH_PROOF\"\n"
	if err := os.WriteFile(filepath.Join(root, "hc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_HERDR_ATTACH_PROOF", marker)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := core.attach(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(b)) != "attach\nt1\n"+socket+"\nunset\nunset\nunset" {
		t.Fatalf("external attach redirected or inherited context: %s %v", b, err)
	}
}
