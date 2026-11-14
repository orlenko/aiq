//go:build darwin || linux

package herdr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNativeCurrentCoreAcceptsLetterBearingAddress(t *testing.T) {
	if _, err := exec.LookPath("hc"); err != nil {
		t.Skip("hc unavailable; positive native origin evidence not run")
	}
	// Each actual native runner has a known PTY. A response naming this parent
	// test process therefore has the wrong TTY, independently of pipe/TTY here.
	fakeSocket, _ := addressEndpoint(t, 1, "fiction-core", "", "live", os.Getpid())
	t.Setenv("GO_HERDR_WRONG_TTY_SOCKET", fakeSocket)
	observed := false
	for i := 0; i < 40; i++ {
		var address string
		ok := t.Run(fmt.Sprint(i), func(t *testing.T) {
			c, _ := fixtureCore(t)
			waitText(t, c, "MISMATCHED TTY REJECTED")
			if err := c.verify(); err != nil {
				t.Fatal("native identity did not remain pinned", err)
			}
			address = c.Terminal
		})
		if !ok {
			return
		} // no retrying a failed startup into a success
		if strings.IndexAny(strings.TrimPrefix(address, "t"), "bcdfghjkmnpqrstvwxyz") >= 0 {
			observed = true
			t.Logf("verified letter-bearing canonical address %s", address)
			break
		}
	}
	if !observed {
		t.Fatal("no letter-bearing canonical terminal was actually verified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHerdrFixture$", "--", "notty")
	cmd.Env = append(os.Environ(), "GO_WANT_HERDR_FIXTURE=1")
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "NATIVE NONTERMINAL REFUSED") {
		t.Fatalf("piped fixture did not reject nonterminal supervision: %v", err)
	}
}
