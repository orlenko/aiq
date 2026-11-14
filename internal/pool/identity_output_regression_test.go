package pool

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdentityOutputNativeHelper(t *testing.T) {
	mode := os.Getenv("AIQ_TEST_IDENTITY_OUTPUT_CLASS")
	if mode == "" {
		return
	}
	if err := os.WriteFile(os.Getenv("AIQ_TEST_IDENTITY_READY"), []byte("entered"), 0600); err != nil {
		os.Exit(2)
	}
	n := 65536
	if strings.HasPrefix(mode, "over-") {
		n++
	}
	fmt.Fprint(os.Stdout, strings.Repeat("x", n))
	if strings.HasSuffix(mode, "held") {
		for {
			time.Sleep(time.Second)
		}
	}
	if strings.HasSuffix(mode, "failure") {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestIdentityOutputNativeCopyClass(t *testing.T) {
	if _, bypass := any(&identityOutput{}).(io.ReaderFrom); bypass {
		t.Error("stdout writer exposes a copy path that bypasses bounded Write")
	}
	for _, mode := range []string{"exact-success", "exact-failure", "over-success", "over-failure", "over-held"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			t.Cleanup(cancel)
			ready := filepath.Join(t.TempDir(), "invented-query-ready")
			var cmd *exec.Cmd
			var child context.Context
			hostnameCalls := 0
			sources := identitySources{
				machine: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("")), nil },
				probe: func(c context.Context) ([]byte, error) {
					return runIdentityProbe(c, func(query context.Context) *exec.Cmd {
						child = query
						cmd = exec.CommandContext(query, os.Args[0], "-test.run=^TestIdentityOutputNativeHelper$")
						cmd.Env = append(os.Environ(), "AIQ_TEST_IDENTITY_OUTPUT_CLASS="+mode, "AIQ_TEST_IDENTITY_READY="+ready)
						return cmd
					})
				},
				hostname: func() (string, error) { hostnameCalls++; return "invented-fallback", nil },
			}
			id, err := readIdentity(ctx, sources)
			if _, e := os.Stat(ready); e != nil {
				t.Fatal("invented query did not enter")
			}
			joined := cmd != nil && cmd.ProcessState != nil
			t.Logf("mode=%s fallback_calls=%d fatal=%t joined=%t child_cause=%v", mode, hostnameCalls, err == ErrIdentityUnavailable, joined, child.Err())
			if !joined {
				t.Fatal("query process not joined")
			}
			if strings.HasPrefix(mode, "exact-") {
				if err != nil || id != "invented-fallback" || hostnameCalls != 1 {
					t.Fatal("ordinary bounded query fallback changed")
				}
				return
			}
			if err != ErrIdentityUnavailable || id != "" || hostnameCalls != 0 {
				t.Error("overflow must be fatal regardless of query exit; fallback forbidden")
			}
			if mode == "over-held" {
				deadline, _ := child.Deadline()
				if child.Err() != context.Canceled || time.Until(deadline) < 250*time.Millisecond {
					t.Error("held overflow returned only at ordinary deadline instead of canceling promptly")
				}
			}
		})
	}
}
