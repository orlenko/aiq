package terminal

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }
func TestObservationTmuxExactIdentityAndUnknownFailures(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		err       error
		want      Observation
	}{
		{"running", "%17\t2468\t0", nil, Running}, {"ended", "%17\t2468\t1", nil, Ended},
		{"wrong pane", "%18\t2468\t0", nil, Unknown}, {"wrong pid", "%17\t9999\t0", nil, Unknown},
		{"missing", "\t\t", nil, Unknown}, {"empty", "", nil, Unknown}, {"extra fields", "%17\t2468\t0\tother", nil, Unknown},
		{"negative", "%17\t-1\t0", nil, Unknown}, {"pid alias", "%17\t02468\t0", nil, Unknown}, {"pid overflow", "%17\t4294967296\t0", nil, Unknown},
		{"unknown state", "%17\t2468\t2", nil, Unknown}, {"multi line", "%17\t2468\t0\n%18\t9999\t0", nil, Unknown},
		{"oversize", strings.Repeat("x", 1025), nil, Unknown}, {"query failure", "", errors.New("INVENTED-PRIVATE-DETAIL"), Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := observationDeps{tmux: func(ctx context.Context, ref string) (string, error) {
				calls++
				if ref != "%17" {
					t.Fatal("query target changed")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > observationTimeout {
					t.Fatal("owned query lacks bound")
				}
				return tc.out, tc.err
			}}
			if got := observeWith(context.Background(), ObservationTarget{Ref: "%17", PID: 2468}, d); got != tc.want || calls != 1 {
				t.Fatalf("state%d calls%d", got, calls)
			}
		})
	}
	for _, ref := range []string{"", "%", "%01", "%-1", "%1;kill-server", "-t", "herdr:invented", "%18446744073709551616", "%17\n"} {
		calls := 0
		if got := observeWith(context.Background(), ObservationTarget{Ref: ref, PID: 2468}, observationDeps{tmux: func(context.Context, string) (string, error) { calls++; return "", nil }}); got != Unknown || calls != 0 {
			t.Fatal("invalid target queried")
		}
	}
}
func TestObservationHerdrRequiresSavedGenerationAndLaunchIdentity(t *testing.T) {
	family := herdr.Prefix + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 32)
	target := ObservationTarget{Ref: family + "@7", Workspace: "/invented/work", PID: 2468, LeaseID: 11, LaunchID: 22}
	for _, mode := range []string{"running", "ended", "generation", "family", "pid", "workspace", "lease", "launch", "starting", "stopping", "lost", "error"} {
		t.Run(mode, func(t *testing.T) {
			r := herdr.Record{Ref: family, Generation: 7, PID: 2468, Workspace: target.Workspace, LeaseID: 11, LaunchID: 22, State: "running"}
			var err error
			want := Unknown
			switch mode {
			case "running":
				want = Running
			case "ended":
				r.State = "ended"
				want = Ended
			case "generation":
				r.Generation = 8
			case "family":
				r.Ref = herdr.Prefix + strings.Repeat("c", 32) + "-" + strings.Repeat("d", 32)
			case "pid":
				r.PID++
			case "workspace":
				r.Workspace = "/invented/other"
			case "lease":
				r.LeaseID++
			case "launch":
				r.LaunchID++
			case "error":
				err = herdr.ErrEnded
			default:
				r.State = mode
			}
			d := observationDeps{herdr: func(ref string, timeout time.Duration) (herdr.Record, error) {
				if ref != target.Ref || timeout <= 0 || timeout > observationTimeout {
					t.Fatal("query identity/deadline")
				}
				return r, err
			}}
			if got := observeWith(context.Background(), target, d); got != want {
				t.Fatalf("state%d wanted%d", got, want)
			}
		})
	}
	for _, change := range []func(*ObservationTarget){func(v *ObservationTarget) { v.Ref = family }, func(v *ObservationTarget) { v.PID = 0 }, func(v *ObservationTarget) { v.LeaseID = 0 }, func(v *ObservationTarget) { v.LaunchID = 0 }, func(v *ObservationTarget) { v.Workspace = "" }} {
		v := target
		change(&v)
		calls := 0
		d := observationDeps{herdr: func(string, time.Duration) (herdr.Record, error) { calls++; return herdr.Record{}, nil }}
		if observeWith(context.Background(), v, d) != Unknown || calls != 0 {
			t.Fatal("unbound generation queried")
		}
	}
}
func TestObservationCancellationIsUnknownAndOwnsOnlyTheQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	d := observationDeps{tmux: func(context.Context, string) (string, error) { calls++; return "", nil }}
	if observeWith(ctx, ObservationTarget{Ref: "%17", PID: 2468}, d) != Unknown || calls != 0 {
		t.Fatal("cancelled request queried")
	}
	if observeWith(nil, ObservationTarget{Ref: "%17", PID: 2468}, d) != Unknown {
		t.Fatal("nil context")
	}
	oversize := int64(math.MaxInt32) + 1
	if observeWith(context.Background(), ObservationTarget{Ref: "%17", PID: int(oversize)}, d) != Unknown || calls != 0 {
		t.Fatal("unrepresentable pid queried")
	}
	parent, stop := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer stop()
	started := time.Now()
	d.tmux = func(child context.Context, _ string) (string, error) { <-child.Done(); return "", child.Err() }
	if observeWith(parent, ObservationTarget{Ref: "%17", PID: 2468}, d) != Unknown || time.Since(started) > time.Second {
		t.Fatal("cancelled query did not settle")
	}
}
func TestObservationActualOwnedCommandHasFixedArgumentsAndBoundedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX query fixture NOT RUN on Windows")
	}
	root := t.TempDir()
	args := filepath.Join(root, "args")
	t.Setenv("AIQ_TEST_OBSERVATION_ARGS", args)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AIQ_TEST_OBSERVATION_ARGS\"\ncase \"$AIQ_TEST_OBSERVATION_MODE\" in\nhuge) printf '%02048d' 0;;\nstall) exec /bin/sleep 2;;\n*) printf '%%17\\t2468\\t0\\n';;\nesac\n"
	if err := os.WriteFile(filepath.Join(root, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if Observe(context.Background(), ObservationTarget{Ref: "%17", PID: 2468}) != Running {
		t.Fatal("invented query failed")
	}
	raw, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "display-message\n-p\n-t\n%17\n#{pane_id}\t#{pane_pid}\t#{pane_dead}\n" {
		t.Fatal("query arguments changed")
	}
	for _, mode := range []string{"huge", "stall"} {
		t.Setenv("AIQ_TEST_OBSERVATION_MODE", mode)
		start := time.Now()
		if Observe(context.Background(), ObservationTarget{Ref: "%17", PID: 2468}) != Unknown || time.Since(start) > time.Second {
			t.Fatal("unbounded or falsely definitive query")
		}
	}
}
