//go:build darwin || linux

package herdr

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// TestHerdrFixture is a provider-free terminal program. The helper branch
// exits before normal package tests and inherits only testenv's scratch homes.
func TestHerdrFixture(t *testing.T) {
	if os.Getenv("GO_WANT_HERDR_FIXTURE") != "1" {
		return
	}
	role := os.Args[len(os.Args)-1]
	switch role {
	case "runner":
		core, err := CurrentCore()
		if err != nil {
			fmt.Println("FIXTURE ERROR", err)
			os.Exit(1)
		}
		if fakeSocket := os.Getenv("GO_HERDR_WRONG_TTY_SOCKET"); fakeSocket != "" {
			originalSocket := os.Getenv("HC_SOCKET")
			os.Setenv("HC_SOCKET", fakeSocket)
			_, wrongErr := CurrentCore()
			os.Setenv("HC_SOCKET", originalSocket)
			if wrongErr == nil || !strings.Contains(wrongErr.Error(), "this TTY") {
				fmt.Println("FIXTURE ERROR wrong TTY was not rejected at its guard")
				os.Exit(2)
			}
			fmt.Println("MISMATCHED TTY REJECTED")
		}
		ws := os.Getenv("GO_HERDR_WORKSPACE")
		before, _ := fixtureMode()
		code, err := Run(Options{Core: core, Workspace: ws, Session: "aiq-fixture", Dir: ws,
			Args: []string{os.Args[0], "-test.run=^TestHerdrFixture$", "--", "child"},
			Hook: func(pid int, g uint64, h Hook) []byte {
				fmt.Printf("HOOK %d %d %s\n", pid, g, h.Event)
				if h.Event == "barrier" {
					os.WriteFile(filepath.Join(ws, "hook-entered"), []byte("ready"), 0600)
					until := time.Now().Add(5 * time.Second)
					for time.Now().Before(until) {
						if _, err := os.Stat(filepath.Join(ws, "release-hook")); err == nil {
							break
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
				return nil
			}})
		if err != nil {
			fmt.Println("FIXTURE ERROR", err)
			os.Exit(1)
		}
		after, _ := fixtureMode()
		// PENDIN is kernel input-retype state, not the terminal policy we
		// restore. Darwin sets it when returning from raw to cooked input.
		before.Lflag &^= unix.PENDIN
		after.Lflag &^= unix.PENDIN
		if !reflect.DeepEqual(before, after) {
			fmt.Println("MODE NOT RESTORED")
			os.Exit(1)
		}
		fmt.Println("MODE RESTORED")
		os.Exit(code)
	case "notty":
		if _, err := Run(Options{}); err == nil || !strings.Contains(err.Error(), "terminal stdin and stdout") {
			fmt.Println("FIXTURE ERROR nonterminal admission")
			os.Exit(2)
		}
		fmt.Println("NATIVE NONTERMINAL REFUSED")
		os.Exit(0)
	case "descendant":
		signal.Ignore(syscall.SIGTERM)
		fmt.Printf("DESCENDANT %d\n", os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	case "child":
		ref := os.Getenv("AIQ_TERMINAL")
		g, _ := strconv.ParseUint(os.Getenv("AIQ_TERMINAL_GENERATION"), 10, 64)
		if err := Register(ref, g, int64(g), int64(g), "claude,codex"); err != nil {
			fmt.Println("REGISTER ERROR", err)
			os.Exit(1)
		}
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, os.Interrupt, syscall.SIGWINCH)
		go func() {
			for sig := range sigs {
				if sig == os.Interrupt {
					fmt.Println("INTERRUPT")
				} else {
					w, h, _ := term.GetSize(int(os.Stdout.Fd()))
					fmt.Printf("RESIZE %d %d\n", w, h)
				}
			}
		}()
		fmt.Printf("NATIVE READY %d %d\n", g, os.Getpid())
		reader := bufio.NewScanner(os.Stdin)
		for reader.Scan() {
			line := reader.Text()
			switch line {
			case "quit":
				os.Exit(0)
			case "rawquit":
				term.MakeRaw(int(os.Stdin.Fd()))
				os.Exit(0)
			case "fork":
				cmd := exec.Command(os.Args[0], "-test.run=^TestHerdrFixture$", "--", "descendant")
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
				if err := cmd.Start(); err != nil {
					fmt.Println("FORK ERROR", err)
				}
			case "hook":
				ReportHook(ref, g, os.Getpid(), Hook{Provider: "codex", Event: "userpromptsubmit", Payload: json.RawMessage(`{}`), LeaseID: int64(g)})
			default:
				fmt.Println("ECHO", line)
			}
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func fixtureCore(t *testing.T) (Core, string) {
	t.Helper()
	if _, err := exec.LookPath("hc"); err != nil {
		t.Skip("hc not installed; real core/PTY evidence unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "aiqh-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("HC_SOCKET", filepath.Join(root, "core.sock"))
	t.Setenv("HC_SESSION", "aiq-native-fixture")
	t.Setenv("GO_WANT_HERDR_FIXTURE", "1")
	t.Setenv("GO_HERDR_WORKSPACE", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "aiq"))
	// All XDG homes were set by testenv; an explicit short socket keeps
	// these real core tests separate even on macOS's long temporary paths.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "hc", "server", "stop", "--json")
		cmd.WaitDelay = time.Second
		if _, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("private core cleanup failed: %v", err)
		}
	})
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLaunch()
	cmd := exec.CommandContext(launchCtx, "hc", "run", "--bg", "--json", "--no-parent", "--cwd", root, "--", os.Args[0], "-test.run=^TestHerdrFixture$", "--", "runner")
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scratch core launch: %v: %s", err, out)
	}
	var launched struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(out, &launched); err != nil || launched.Address == "" {
		t.Fatalf("scratch launch result: %s", out)
	}
	var hello struct {
		Core struct {
			ID string `json:"id"`
		} `json:"core"`
	}
	if err := exchange(os.Getenv("HC_SOCKET"), "hello", map[string]any{}, &hello, callTimeout); err != nil {
		t.Fatal(err)
	}
	var terminal coreTerminal
	if err := exchange(os.Getenv("HC_SOCKET"), "terminal.get", map[string]any{"terminal": launched.Address}, &terminal, callTimeout); err != nil {
		t.Fatal(err)
	}
	core := Core{Socket: os.Getenv("HC_SOCKET"), ID: hello.Core.ID, Terminal: launched.Address, PID: terminal.PID}
	waitText(t, core, "NATIVE READY 1")
	return core, root
}

func waitText(t *testing.T, c Core, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var text string
	for time.Now().Before(deadline) {
		var r struct {
			Text string `json:"text"`
		}
		if err := exchange(c.Socket, "terminal.read", map[string]any{"terminal": c.Terminal, "lines": 50}, &r, time.Second); err == nil {
			text = r.Text
			if strings.Contains(text, want) {
				return text
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("scratch terminal never printed %q; screen: %s", want, text)
	return ""
}

func sendFixture(t *testing.T, c Core, line string) {
	t.Helper()
	if err := exchange(c.Socket, "terminal.send", map[string]any{"terminal": c.Terminal, "text": line}, nil, 12*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCoreReplacementAndExit(t *testing.T) {
	c, ws := fixtureCore(t)
	r, owned, err := Workspace(ws)
	if err != nil || !owned {
		t.Fatalf("ownership: %v %v", owned, err)
	}
	sendFixture(t, c, "hello native")
	waitText(t, c, "ECHO hello native")
	if err := c.keys([]string{"ctrl+c"}); err != nil {
		t.Fatal(err)
	}
	waitText(t, c, "INTERRUPT")
	sendFixture(t, c, "fork")
	waitText(t, c, "DESCENDANT")
	old := r
	if err := Replace(r.Target(), r.PID, ws, []string{os.Args[0], "-test.run=^TestHerdrFixture$", "--", "child"}); err != nil {
		t.Fatal(err)
	}
	waitText(t, c, "NATIVE READY 2")
	r, err = Info(r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ref != old.Ref || r.Core != old.Core || r.PID == old.PID || r.Generation != old.Generation+1 {
		t.Fatalf("terminal/generation changed incorrectly: old=%+v new=%+v", old, r)
	}
	if active, err := groupActive(old.PID); err != nil || active {
		t.Fatalf("old executable group survived replacement: %v %v", active, err)
	}
	if err := Replace(old.Target(), old.PID, ws, []string{os.Args[0], "-test.run=^TestHerdrFixture$", "--", "child"}); err == nil {
		t.Fatal("replayed old replacement accepted")
	}
	if _, err := ReportHook(old.Ref, old.Generation, old.PID, Hook{Provider: "codex", Event: "stop", Payload: json.RawMessage(`{}`), LeaseID: 1}); err == nil {
		t.Fatal("old hook accepted into new generation")
	}
	sendFixture(t, c, "hook")
	waitText(t, c, fmt.Sprintf("HOOK %d 2 userpromptsubmit", r.PID))
	// A temporary control failure retains ownership and does not free the
	// workspace while its actual child still runs.
	if err := os.Rename(r.Socket, r.Socket+".offline"); err != nil {
		t.Fatal(err)
	}
	_, owned, err = Workspace(ws)
	if err == nil || !owned {
		t.Fatal("control failure became an empty workspace")
	}
	if err := os.Rename(r.Socket+".offline", r.Socket); err != nil {
		t.Fatal(err)
	}
	sendFixture(t, c, "rawquit")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var terminal coreTerminal
		if e := exchange(c.Socket, "terminal.get", map[string]any{"terminal": c.Terminal}, &terminal, time.Second); e == nil && terminal.State == "finished" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	ended, err := loadKey(workspaceKey(ws))
	if err != nil || ended.State != "ended" {
		text := waitText(t, c, "NATIVE READY 2")
		t.Fatalf("ordinary exit did not end: %+v %v; screen=%s", ended, err, text)
	}
	text := waitText(t, c, "MODE RESTORED")
	if strings.Contains(text, "NATIVE READY 3") {
		t.Fatal("ordinary exit restarted the provider")
	}
	if !strings.Contains(text, "ECHO hello native") {
		t.Fatal("terminal history was lost across native replacement/exit")
	}
}

func TestNativeStopIsFinal(t *testing.T) {
	c, ws := fixtureCore(t)
	r, _, err := Workspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := Stop(r.Target(), r.PID); err != nil {
		t.Fatal(err)
	}
	ended, err := loadKey(workspaceKey(ws))
	if err != nil || ended.State != "ended" {
		t.Fatalf("stop not recorded: %+v %v", ended, err)
	}
	text := waitText(t, c, "NATIVE READY 1")
	if strings.Contains(text, "NATIVE READY 2") {
		t.Fatal("intentional stop restarted provider")
	}
}

// A tiny real viewer exercises core's public framed attach protocol. Closing
// the viewer is distinct from killing the terminal; resize/input reach the
// foreground child directly through the shared PTY.
type testViewer struct {
	net.Conn
	channel uint32
	reader  *bufio.Reader
}

func attachViewer(t *testing.T, c Core) *testViewer {
	t.Helper()
	conn, err := net.DialTimeout("unix", c.Socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	json.NewEncoder(conn).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "hello", "params": map[string]bool{"framed": true}})
	if _, err := r.ReadBytes('\n'); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	v := &testViewer{Conn: conn, reader: r}
	q, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "terminal.attach", "params": map[string]any{"terminal": c.Terminal, "cols": 100, "rows": 36}})
	v.frame(t, 0, 0, q)
	for {
		var header [9]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		n := binary.LittleEndian.Uint32(header[5:])
		if n > maxMessage {
			conn.Close()
			t.Fatal("oversized fixture frame")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if binary.LittleEndian.Uint32(header[:4]) == 0 && header[4] == 0 {
			var out struct {
				Result struct {
					Channel uint32 `json:"channel"`
				} `json:"result"`
			}
			if json.Unmarshal(payload, &out) != nil || out.Result.Channel == 0 {
				conn.Close()
				t.Fatalf("fixture attach failed: %s", payload)
			}
			v.channel = out.Result.Channel
			return v
		}
	}
}
func (v *testViewer) frame(t *testing.T, ch uint32, kind byte, payload []byte) {
	t.Helper()
	b := make([]byte, 9+len(payload))
	binary.LittleEndian.PutUint32(b[:4], ch)
	b[4] = kind
	binary.LittleEndian.PutUint32(b[5:9], uint32(len(payload)))
	copy(b[9:], payload)
	if _, err := v.Write(b); err != nil {
		t.Fatal(err)
	}
}
func (v *testViewer) resize(t *testing.T, w, h uint16) {
	var size [4]byte
	binary.LittleEndian.PutUint16(size[:2], w)
	binary.LittleEndian.PutUint16(size[2:], h)
	v.frame(t, v.channel, 3, size[:])
}

func TestNativeViewerResizeReconnectAndCoreRestart(t *testing.T) {
	c, ws := fixtureCore(t)
	old, _, err := Workspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	v := attachViewer(t, c)
	v.resize(t, 101, 37)
	waitText(t, c, "RESIZE 101 37")
	v.frame(t, v.channel, 0, []byte("from keyboard\n"))
	waitText(t, c, "ECHO from keyboard")
	v.Close()
	if current, err := Info(old.Ref); err != nil || current.PID != old.PID {
		t.Fatalf("viewer detach ended/replaced child: %+v %v", current, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "hc", "server", "restart", "--json").CombinedOutput(); err != nil {
		t.Fatalf("private core restart: %v %s", err, out)
	}
	if current, err := Info(old.Ref); err != nil || current.PID != old.PID {
		t.Fatalf("core restart ended/replaced child: %+v %v", current, err)
	}
	v = attachViewer(t, c)
	defer v.Close()
	v.resize(t, 102, 38)
	waitText(t, c, "RESIZE 102 38")
	v.frame(t, v.channel, 0, []byte("after reconnect\n"))
	waitText(t, c, "ECHO after reconnect")
	if err := Stop(old.Target(), old.PID); err != nil {
		t.Fatal(err)
	}
}

// Hold the actor in a callback, queue replacement, then exit the leader.
// The observer and replacement are both ready when the callback returns.
func TestNativeQueuedReplacementCannotReviveExitedLeader(t *testing.T) {
	for _, descendant := range []bool{false, true} {
		t.Run(fmt.Sprint(descendant), func(t *testing.T) {
			c, ws := fixtureCore(t)
			r, _, err := Workspace(ws)
			if err != nil {
				t.Fatal(err)
			}
			if descendant {
				sendFixture(t, c, "fork")
				waitText(t, c, "DESCENDANT")
			}
			hookDone := make(chan error, 1)
			go func() {
				_, err := ReportHook(r.Ref, r.Generation, r.PID, Hook{Provider: "codex", Event: "barrier", Payload: json.RawMessage(`{}`)})
				hookDone <- err
			}()
			until := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(ws, "hook-entered")); err == nil {
					break
				}
				if time.Now().After(until) {
					t.Fatal("hook barrier not reached")
				}
				time.Sleep(5 * time.Millisecond)
			}
			replacement := make(chan error, 1)
			go func() {
				replacement <- Replace(r.Target(), r.PID, ws, []string{os.Args[0], "-test.run=^TestHerdrFixture$", "--", "child"})
			}()
			if err := syscall.Kill(r.PID, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			// Positive snapshot proves the leader exited before releasing the actor.
			for {
				_, leader, err := groupSnapshot(r.PID)
				if err != nil {
					t.Fatal(err)
				}
				if !leader {
					break
				}
				if time.Now().After(until) {
					t.Fatal("leader did not exit")
				}
				time.Sleep(5 * time.Millisecond)
			}
			os.WriteFile(filepath.Join(ws, "release-hook"), nil, 0600)
			<-hookDone
			<-replacement // Either final reply or closed control is safe; no successor.
			waitText(t, c, "MODE RESTORED")
			ended, err := loadKey(workspaceKey(ws))
			if err != nil || ended.State != "ended" || ended.Generation != 1 {
				t.Fatalf("exited child revived: %+v %v", ended, err)
			}
			if active, err := groupActive(r.PID); err != nil || active {
				t.Fatalf("descendant survived finality: %v %v", active, err)
			}
		})
	}
}

// Simulate the wait observer not having been scheduled yet. A zombie leader
// with executable descendants is still a final ordinary exit.
func TestNativeReplacementChecksLeaderBeforeObserver(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 & echo $! > \"$1\"; exit 0", "sh", filepath.Join(root, "descendant"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()
	if err := waitExited(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	s := session{r: Record{PID: cmd.Process.Pid}, cmd: cmd, exited: make(chan error, 1)}
	active, leader, err := groupSnapshot(cmd.Process.Pid)
	if err != nil || !active || leader {
		t.Fatalf("fixture lacks exited leader plus living group: %v %v %v", active, leader, err)
	}
	if live, err := s.replacementLive(); err != nil || live {
		t.Fatalf("observer lag revived exited leader: %v %v", live, err)
	}
}
