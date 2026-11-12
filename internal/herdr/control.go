package herdr

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/filelock"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
)

const Prefix = "herdr:"

var ErrEnded = errors.New("Herdr session ended")

// Record has no command, environment, credential or transcript contents.
// One file per workspace retains an ended tombstone until its next start.
type Record struct {
	Ref         string
	Workspace   string
	Session     string
	Socket      string
	RunnerPID   int
	PID         int
	Generation  uint64
	State       string
	Core        Core
	LeaseID     int64
	LaunchID    int64
	Fallback    string
	Label       string
	PreviousPID int
}

func workspaceKey(ws string) string {
	sum := sha256.Sum256([]byte(ws))
	return hex.EncodeToString(sum[:16])
}
func directory() string            { return filepath.Join(paths.DataDir(), "herdr") }
func recordPath(key string) string { return filepath.Join(directory(), key+".json") }

func refKey(ref string) (string, error) {
	s, ok := strings.CutPrefix(ref, Prefix)
	if !ok || len(s) != 65 || s[32] != '-' {
		return "", errors.New("invalid Herdr session reference")
	}
	if _, err := hex.DecodeString(s[:32] + s[33:]); err != nil {
		return "", errors.New("invalid Herdr session reference")
	}
	return s[:32], nil
}

func IsRef(ref string) bool { return strings.HasPrefix(ref, Prefix) }

func newRef(ws string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return Prefix + workspaceKey(ws) + "-" + hex.EncodeToString(nonce[:]), nil
}

func loadKey(key string) (Record, error) {
	fi, err := os.Lstat(recordPath(key))
	if err != nil {
		return Record{}, err
	}
	if !fi.Mode().IsRegular() {
		return Record{}, errors.New("Herdr ownership descriptor is not a regular file")
	}
	f, err := os.Open(recordPath(key))
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	var r Record
	if err != nil || len(b) > 64*1024 || json.Unmarshal(b, &r) != nil {
		return r, errors.New("unreadable Herdr workspace ownership; inspect it before starting another session")
	}
	k, err := refKey(r.Ref)
	if err != nil || k != key || workspaceKey(r.Workspace) != key || !filepath.IsAbs(r.Workspace) || !filepath.IsAbs(r.Socket) || r.RunnerPID <= 0 || !filepath.IsAbs(r.Core.Socket) || r.Core.ID == "" || !terminalID.MatchString(r.Core.Terminal) || r.Core.PID <= 0 {
		return r, errors.New("invalid Herdr workspace ownership; refusing another start")
	}
	return r, nil
}

func load(ref string) (Record, error) {
	k, err := refKey(ref)
	if err != nil {
		return Record{}, err
	}
	r, err := loadKey(k)
	if err != nil {
		return r, err
	}
	if r.Ref != ref || r.State == "ended" {
		return r, ErrEnded
	}
	return r, nil
}

func save(r Record) error {
	k, err := refKey(r.Ref)
	if err != nil || k != workspaceKey(r.Workspace) {
		return errors.New("invalid Herdr ownership update")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(recordPath(k), b)
}

// LockWorkspace serializes startup decisions across both terminal backends.
// It must be released before attaching or running the long session itself.
func LockWorkspace(ws string) (func(), error) {
	if err := os.MkdirAll(directory(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(directory(), workspaceKey(ws)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := filelock.TryExclusive(f); err != nil {
		f.Close()
		return nil, errors.New("another long-session start is in progress for this workspace; try again shortly")
	}
	return func() { f.Close() }, nil
}

// Workspace returns existing ownership even if control is unreachable. Callers
// must not convert that error to an empty workspace and create a duplicate.
func Workspace(ws string) (Record, bool, error) {
	r, err := loadKey(workspaceKey(ws))
	if os.IsNotExist(err) {
		return r, false, nil
	}
	if err != nil {
		return Record{}, true, err
	}
	if r.Workspace != ws {
		return r, true, errors.New("Herdr workspace identity mismatch")
	}
	if r.State == "ended" {
		return r, false, nil
	}
	live, err := Info(r.Ref)
	if err != nil {
		return r, true, err
	}
	return live, true, err
}

type Hook struct {
	Ref      string
	Provider string
	Event    string
	Payload  json.RawMessage
	LeaseID  int64
	LaunchID int64
}

type request struct {
	Ref        string
	PID        int
	Generation uint64
	Dir        string
	Args       []string
	LeaseID    int64
	LaunchID   int64
	Fallback   string
	Hook       *Hook
	Label      string
}

type response struct {
	Record Record
	Output []byte
	Error  string
}

func call(ref, op string, q request) (response, error) { return callFor(ref, op, q, callTimeout) }

func callFor(ref, op string, q request, timeout time.Duration) (response, error) {
	r, err := load(ref)
	if err != nil {
		return response{}, err
	}
	q.Ref = ref
	var out response
	err = exchange(r.Socket, op, q, &out, timeout)
	if err == nil && out.Error != "" {
		err = errors.New(out.Error)
	}
	if err == nil && out.Record.Ref != ref {
		err = errors.New("Herdr runner identity mismatch")
	}
	return out, err
}

func Info(ref string) (Record, error) { r, err := call(ref, "info", request{}); return r.Record, err }

// Replace is conditional on the exact child; a stale or repeated request can
// never terminate a newer generation. Success means process started, not ready.
func Replace(ref string, pid int, dir string, args []string) error {
	r, err := Info(ref)
	if err != nil {
		return err
	}
	if r.PID != pid {
		return errors.New("Herdr child changed; reconcile its lease before replacing it")
	}
	_, err = call(ref, "replace", request{PID: pid, Generation: r.Generation, Dir: dir, Args: args})
	return err
}

func Stop(ref string, pid int) error {
	r, err := Info(ref)
	if err != nil {
		return err
	}
	if pid != 0 && r.PID != pid {
		return errors.New("Herdr child changed; stop its current lease instead")
	}
	_, err = call(ref, "stop", request{PID: r.PID, Generation: r.Generation})
	if err != nil {
		if _, e := load(ref); errors.Is(e, ErrEnded) {
			return nil
		}
	}
	return err
}

func Register(ref string, generation uint64, leaseID, launchID int64, fallback string) error {
	_, err := call(ref, "register", request{PID: os.Getpid(), Generation: generation, LeaseID: leaseID, LaunchID: launchID, Fallback: fallback})
	return err
}

func ReportHook(ref string, generation uint64, pid int, h Hook) ([]byte, error) {
	r, err := callFor(ref, "hook", request{PID: pid, Generation: generation, Hook: &h}, 8*time.Second)
	return r.Output, err
}

func Attach(ref string) error {
	r, err := Info(ref)
	if err != nil {
		return err
	}
	return r.Core.attach()
}
func NameFor(ref string, pid int, name string) error {
	r, err := Info(ref)
	if err != nil {
		return err
	}
	if r.PID != pid {
		return errors.New("Herdr child changed")
	}
	_, err = call(ref, "label", request{PID: pid, Generation: r.Generation, Label: name})
	return err
}

// Records is only local runner metadata, never a listing of private Herdr
// terminals. Ended/superseded descriptors need no socket query.
func Records() ([]Record, error) {
	es, err := os.ReadDir(directory())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(es) > 20000 {
		return nil, errors.New("too many Herdr workspace records")
	}
	var out []Record
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".json")
		if len(key) != 32 {
			continue
		}
		if _, err := hex.DecodeString(key); err != nil {
			continue
		}
		r, err := loadKey(key)
		if err != nil {
			return nil, fmt.Errorf("Herdr ownership cannot be read: %w", err)
		}
		if r.State != "ended" {
			out = append(out, r)
		}
	}
	return out, nil
}

// ChildEnv removes inherited routing/bypass/exec-chain state from a persistent
// core or routed parent. Only the data/config selectors survive the AIQ prefix.
func ChildEnv(env []string, ref string, generation uint64) []string {
	blocked := map[string]bool{"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "CLAUDE_CODE_OAUTH_TOKEN": true, "OPENAI_API_KEY": true, "CODEX_API_KEY": true, "GEMINI_API_KEY": true, "GOOGLE_API_KEY": true, "GOOGLE_APPLICATION_CREDENTIALS": true, "COPILOT_API_KEY": true, "COPILOT_GITHUB_TOKEN": true, "TMUX": true, "TMUX_PANE": true, "STY": true}
	out := without(env, func(k string) bool {
		return blocked[k] || (strings.HasPrefix(k, "AIQ_") && k != "AIQ_DATA_DIR" && k != "AIQ_CONFIG")
	})
	for i, kv := range out {
		k, v, ok := strings.Cut(kv, "=")
		if ok && (k == "AIQ_DATA_DIR" || k == "AIQ_CONFIG") && v != "" && !filepath.IsAbs(v) {
			if abs, err := filepath.Abs(v); err == nil {
				out[i] = k + "=" + abs
			}
		}
	}
	return append(out, "AIQ_TERMINAL="+ref, fmt.Sprintf("AIQ_TERMINAL_GENERATION=%d", generation))
}

func Current() string {
	if r := os.Getenv("AIQ_TERMINAL"); IsRef(r) {
		return r
	}
	return os.Getenv("TMUX_PANE")
}

func InfoWithin(ref string, timeout time.Duration) (Record, error) {
	r, err := callFor(ref, "info", request{}, timeout)
	return r.Record, err
}
