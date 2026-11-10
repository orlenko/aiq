//go:build unix

package daemon

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ui-ports keeps the navigator off ports a tab may still point at, so only a file of the
// owner's alone counts, and anything else stops the navigator rather than reading as empty.
func TestThePortListMustBeTheOwnersAlone(t *testing.T) {
	for name, create := range map[string]func(path string) error{
		"open to the group": func(path string) error {
			if err := os.WriteFile(path, []byte("50001 1\n"), 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0o640) // whatever the umask
		},
		"a link": func(path string) error {
			target := path + ".real"
			if err := os.WriteFile(target, []byte("50001 1\n"), 0o600); err != nil {
				return err
			}
			return os.Symlink(target, path)
		},
		"a FIFO": func(path string) error { return syscall.Mkfifo(path, 0o600) },
	} {
		useDataDir(t)
		if err := create(navPortsPath()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			ln, _, err := bindNavigator()
			if err == nil {
				ln.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: bound", name)
			} else if !strings.Contains(err.Error(), "remove or fix") {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the read hangs", name)
		}
	}
}

// A port that cannot be recorded is never bound.
func TestAPortThatCannotBeListedIsNotBound(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := useDataDir(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	saved := listenTCP
	t.Cleanup(func() { listenTCP = saved })
	listenTCP = func(addr string) (net.Listener, error) {
		t.Errorf("bound %s with nothing recorded", addr)
		return nil, errors.New("no bind in this test")
	}
	if _, _, err := bindNavigator(); err == nil {
		t.Fatal("no error")
	}
}
