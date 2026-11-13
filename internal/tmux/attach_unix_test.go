//go:build unix

package tmux

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func attachFakeBinary(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	binary, log := filepath.Join(root, "tmux"), filepath.Join(root, "calls")
	script := `#!/bin/sh
printf 'CALL\000' >> "$GO_ATTACH_ARGS"
printf '%s\000' "$@" >> "$GO_ATTACH_ARGS"
printf 'ENV\000%s\000END\000' "${TMUX-}" >> "$GO_ATTACH_ARGS"
case "$*" in
  *display-message*)
    case "$GO_ATTACH_MODE" in
      blocked) exec /bin/sleep 30 ;;
      failure) printf 'PRIVATE control diagnostic' >&2; exit 1 ;;
    esac
    printf '%s' "$GO_ATTACH_METADATA" ;;
  *) printf 'PRIVATE viewer diagnostic' >&2
    [ "$GO_ATTACH_MODE" = attach-failure ] && exit 1
    ;;
esac
exit 0
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, log
}
func attachFakeCalls(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func TestAttachPreparedFrozenArgvAndEnvironment(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(map[bool]string{false: "outside", true: "inside"}[inside], func(t *testing.T) {
			binary, log := attachFakeBinary(t)
			socket := "/fixture/saved, pipe| socket "
			tmuxContext := ""
			if inside {
				tmuxContext = "/fixture/caller,comma,sock,123,0"
			}
			t.Setenv("PATH", filepath.Dir(binary))
			t.Setenv("TMUX", tmuxContext)
			t.Setenv("GO_ATTACH_ARGS", log)
			t.Setenv("GO_ATTACH_METADATA", "%17|2468|0|$3|@9|"+socket+"\n")
			t.Setenv("GO_ATTACH_MODE", "")
			action, err := PrepareAttach("%17", 2468)
			if err != nil || action == nil {
				t.Fatal(err)
			}
			query := []string{"CALL", "-N"}
			if inside {
				query = append(query, "-S", "/fixture/caller,comma,sock")
			}
			query = append(query, "display-message", "-p", "-t", "%17", "#{pane_id}|#{pane_pid}|#{pane_dead}|#{session_id}|#{window_id}|#{socket_path}", "ENV", tmuxContext, "END")
			if got := attachFakeCalls(t, log); !reflect.DeepEqual(got, query) {
				t.Fatalf("query argv %q want %q", got, query)
			}
			// Preparation alone never performs selection. Later ambient context and
			// PATH changes must not alter the captured operation or executable.
			t.Setenv("TMUX", "/invented/replacement,456,0")
			t.Setenv("PATH", "/invented/no-bin")
			if err := action(); err != nil {
				t.Fatal(err)
			}
			operation := []string{"CALL", "-N", "-S", socket}
			if inside {
				operation = append(operation, "switch-client", "-t", "%17")
			} else {
				operation = append(operation, "select-window", "-t", "%17", ";", "select-pane", "-t", "%17", ";", "attach-session", "-t", "$3")
			}
			operation = append(operation, "ENV", tmuxContext, "END")
			if got, want := attachFakeCalls(t, log), append(query, operation...); !reflect.DeepEqual(got, want) {
				t.Fatalf("action argv %q want %q", got, want)
			}
		})
	}
}

func TestAttachControlBoundsAndNoFallback(t *testing.T) {
	for _, mode := range []string{"failure", "blocked", "oversized", "mismatch", "attach-failure", "invalid-context", "invalid-pane"} {
		t.Run(mode, func(t *testing.T) {
			binary, log := attachFakeBinary(t)
			metadata := "%17|2468|0|$3|@9|/fixture/socket\n"
			if mode == "oversized" {
				metadata = strings.Repeat("x", 65537)
			}
			if mode == "mismatch" {
				metadata = "%17|9999|0|$3|@9|/fixture/socket\n"
			}
			env := []string{"GO_ATTACH_ARGS=" + log, "GO_ATTACH_METADATA=" + metadata, "GO_ATTACH_MODE=" + mode}
			if mode == "invalid-context" {
				env = append(env, "TMUX=relative,123,0")
			}
			pane := "%17"
			if mode == "invalid-pane" {
				pane = "%17;new-session"
			}
			budget := 4 * time.Second
			if mode == "blocked" {
				budget = time.Second
			}
			started := time.Now()
			action, err := prepareAttach(pane, 2468, env, binary, budget)
			elapsed := time.Since(started)
			if mode == "blocked" && (elapsed < budget/2 || elapsed > budget+3*time.Second) {
				t.Fatalf("blocked control elapsed %v, want between %v and %v", elapsed, budget/2, budget+3*time.Second)
			}
			if mode == "attach-failure" {
				if err != nil {
					t.Fatal(err)
				}
				if err := action(); err != ErrAttachUnavailable {
					t.Fatal("interactive failure not classified")
				}
				calls := attachFakeCalls(t, log)
				n := 0
				for _, s := range calls {
					if s == "CALL" {
						n++
					}
				}
				if n != 2 {
					t.Fatal("interactive failure retried or fell back")
				}
				return
			}
			if err == nil || action != nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("unsafe control result", err)
			}
			if mode == "invalid-context" || mode == "invalid-pane" {
				if _, err := os.Stat(log); !os.IsNotExist(err) {
					t.Fatal("invalid input executed backend")
				}
				return
			}
			calls := attachFakeCalls(t, log)
			n := 0
			for _, s := range calls {
				if s == "CALL" {
					n++
				}
			}
			if n != 1 {
				t.Fatal("failed control retried or attached")
			}
		})
	}
}
