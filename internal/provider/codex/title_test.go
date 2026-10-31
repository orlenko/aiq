package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestNewNamedThread(t *testing.T) {
	const name = "Harp 雪 'quoted'"
	home := t.TempDir()
	p := &Provider{Command: func(args, env []string) *exec.Cmd {
		if len(args) != 1 || args[0] != "app-server" {
			t.Fatalf("unexpected command: %q", args)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestNamedThreadServer$")
		cmd.Env = append(env, "AIQ_TEST_NAMED_THREAD=1", "AIQ_TEST_THREAD_NAME="+name, "AIQ_TEST_WORKSPACE="+home)
		return cmd
	}}
	id, err := p.NewNamedThread(home, false, home, name)
	if err != nil || id != "new-thread" {
		t.Fatalf("thread = %q, err = %v", id, err)
	}
}

func TestNewNamedThreadFailsWithoutUsableMetadata(t *testing.T) {
	for _, mode := range []string{"no-id", "name-error"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			p := &Provider{Command: func(args, env []string) *exec.Cmd {
				cmd := exec.Command(os.Args[0], "-test.run=^TestNamedThreadServer$")
				cmd.Env = append(env, "AIQ_TEST_NAMED_THREAD=1", "AIQ_TEST_THREAD_NAME=Harp", "AIQ_TEST_WORKSPACE="+home, "AIQ_TEST_THREAD_ERROR="+mode)
				return cmd
			}}
			if id, err := p.NewNamedThread(home, false, home, "Harp"); err == nil || id != "" {
				t.Fatalf("accepted unusable named thread: %q %v", id, err)
			}
		})
	}
}

// A process-level fake validates the JSON-RPC exchange. It accepts no model
// turn, so a regression that starts one fails rather than consuming quota.
func TestNamedThreadServer(t *testing.T) {
	if os.Getenv("AIQ_TEST_NAMED_THREAD") != "1" {
		return
	}
	step := 0
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.Method == "initialized" {
			continue
		}
		var result any = map[string]any{}
		switch {
		case step == 0 && req.Method == "initialize":
		case step == 1 && req.Method == "thread/start":
			if req.Params["cwd"] != os.Getenv("AIQ_TEST_WORKSPACE") {
				os.Exit(3)
			}
			result = map[string]any{"thread": map[string]any{"id": "new-thread"}}
			if os.Getenv("AIQ_TEST_THREAD_ERROR") == "no-id" {
				result = map[string]any{"thread": map[string]any{}}
			}
		case step == 2 && req.Method == "thread/name/set":
			if req.Params["threadId"] != "new-thread" || req.Params["name"] != os.Getenv("AIQ_TEST_THREAD_NAME") {
				os.Exit(4)
			}
			if os.Getenv("AIQ_TEST_THREAD_ERROR") == "name-error" {
				out, _ := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "name unavailable"}})
				fmt.Println(string(out))
				continue
			}
		default:
			fmt.Fprintln(os.Stderr, "unexpected RPC:", req.Method)
			os.Exit(5)
		}
		step++
		out, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
		fmt.Println(string(out))
	}
	os.Exit(0)
}
