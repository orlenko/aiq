package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdentityContextSourceOrderAndBounds(t *testing.T) {
	for _, tc := range []struct {
		machine, probe, host, want string
		unavailable                bool
	}{
		{" invented-machine-id\n", "ignored", "ignored", "invented-machine-id", false},
		{" ", `"IOPlatformUUID" = "00000000-0000-4000-8000-000000000001"`, "ignored", "00000000-0000-4000-8000-000000000001", false},
		{"", "no UUID", "invented-host", "invented-host", false},
		{strings.Repeat("x", 4097), "", "", "", true},
		{"\xff", "", "", "", true},
		{"", strings.Repeat("x", 65537), "invented-host", "", true},
	} {
		probes, hosts := 0, 0
		s := identitySources{machine: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.machine)), nil }, probe: func(context.Context) ([]byte, error) { probes++; return []byte(tc.probe), nil }, hostname: func() (string, error) { hosts++; return tc.host, nil }}
		id, err := readIdentity(context.Background(), s)
		if id != tc.want || (err != nil) != tc.unavailable {
			t.Fatal("invented identity contract mismatch")
		}
		if tc.want == "invented-machine-id" && (probes != 0 || hosts != 0) {
			t.Fatal("later identity source invoked")
		}
		if tc.unavailable && hosts != 0 {
			t.Fatal("invalid acquisition fell back")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := HostnameContext(ctx); id != "" || err != ErrIdentityUnavailable {
		t.Fatal("canceled public lookup")
	}
	called := false
	s := identitySources{machine: func() (io.ReadCloser, error) { called = true; return nil, errors.New("invented") }, probe: func(context.Context) ([]byte, error) { called = true; return nil, nil }, hostname: func() (string, error) { called = true; return "invented", nil }}
	if _, err := readIdentity(ctx, s); err != ErrIdentityUnavailable || called {
		t.Fatal("canceled lookup had source effects")
	}
	s.machine = func() (io.ReadCloser, error) { return nil, errors.New("invented file absence") }
	s.probe = func(context.Context) ([]byte, error) { return nil, errors.New("invented normal probe failure") }
	if id, err := readIdentity(context.Background(), s); err != nil || id != "invented" {
		t.Fatal("ordinary fallback changed")
	}
	s.probe = func(context.Context) ([]byte, error) { return nil, ErrIdentityUnavailable }
	called = false
	if _, err := readIdentity(context.Background(), s); err != ErrIdentityUnavailable || called {
		t.Fatal("fatal query fell back")
	}
}

func TestIdentityContextProbeHelper(t *testing.T) {
	mode := os.Getenv("AIQ_TEST_IDENTITY_HELPER")
	if mode == "" {
		return
	}
	if err := os.WriteFile(os.Getenv("AIQ_TEST_IDENTITY_READY"), []byte("entered"), 0600); err != nil {
		os.Exit(2)
	}
	switch mode {
	case "positive":
		fmt.Fprint(os.Stdout, `"IOPlatformUUID" = "00000000-0000-4000-8000-000000000001"`)
		os.Exit(0)
	case "overflow":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 65537))
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestIdentityContextOwnedNativeQueryCancellation(t *testing.T) {
	for _, mode := range []string{"positive", "held", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ready := filepath.Join(t.TempDir(), "invented-ready")
			var child *exec.Cmd
			query := func(c context.Context) *exec.Cmd {
				child = exec.CommandContext(c, os.Args[0], "-test.run=^TestIdentityContextProbeHelper$")
				child.Env = append(os.Environ(), "AIQ_TEST_IDENTITY_HELPER="+mode, "AIQ_TEST_IDENTITY_READY="+ready)
				return child
			}
			started := time.Now()
			out, err := runIdentityProbe(ctx, query)
			if _, e := os.Stat(ready); e != nil {
				t.Fatal("invented query never entered")
			}
			if child == nil || child.ProcessState == nil {
				t.Fatal("owned query was not joined")
			}
			if mode == "positive" {
				if err != nil || !strings.Contains(string(out), "IOPlatformUUID") {
					t.Fatal("native query control failed")
				}
				return
			}
			if err != ErrIdentityUnavailable || out != nil || time.Since(started) > 2500*time.Millisecond {
				t.Fatal("owned held/overflow query did not terminate within allowance")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := runIdentityProbe(ctx, func(context.Context) *exec.Cmd { t.Fatal("insufficient cleanup reserve started a query"); return nil }); err != ErrIdentityUnavailable {
		t.Fatal("missing reserve not refused")
	}
}
