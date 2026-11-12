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
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/filelock"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
)

const Prefix = "herdr:"

var ErrEnded = errors.New("Herdr session ended")
var ErrGenerationChanged = errors.New("Herdr terminal has moved to another child generation; use aiq long attach in the workspace to inspect it")

// ParseRef separates stable terminal ownership from an optional immutable
// child generation. A bare family permits observation, never mutation.
func ParseRef(ref string) (family string, generation uint64, err error) {
	family, suffix, tagged := strings.Cut(ref, "@")
	s, ok := strings.CutPrefix(family, Prefix)
	if !ok || len(s) != 65 || s[32] != '-' || strings.ToLower(s) != s {
		return "", 0, errors.New("invalid Herdr session reference")
	}
	if _, e := hex.DecodeString(s[:32] + s[33:]); e != nil {
		return "", 0, errors.New("invalid Herdr session reference")
	}
	if tagged {
		generation, err = strconv.ParseUint(suffix, 10, 64)
		if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != suffix {
			return "", 0, errors.New("invalid Herdr child generation")
		}
	}
	return family, generation, nil
}

// Target is the opaque lease identity of this snapshot's child. Records keep
// their stable Ref while every child gets a different target.
func (r Record) Target() string { return r.Ref + "@" + strconv.FormatUint(r.Generation, 10) }

func mutationGeneration(ref string) (uint64, error) {
	_, g, err := ParseRef(ref)
	if err == nil && g == 0 {
		err = errors.New("Herdr mutation requires a saved child generation; inspect the original terminal")
	}
	return g, err
}

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
	family, g, err := ParseRef(ref)
	if err != nil || g != 0 {
		return "", errors.New("invalid Herdr ownership reference")
	}
	return strings.TrimPrefix(family, Prefix)[:32], nil
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
	family, _, err := ParseRef(ref)
	if err != nil {
		return Record{}, err
	}
	k, _ := refKey(family)
	r, err := loadKey(k)
	if err != nil {
		return r, err
	}
	if r.Ref != family || r.State == "ended" {
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
	family, generation, err := ParseRef(ref)
	if err != nil {
		return response{}, err
	}
	if op != "info" && generation != 0 && generation != q.Generation {
		return response{}, ErrGenerationChanged
	}
	r, err := load(family)
	if err != nil {
		return response{}, err
	}
	q.Ref = family
	var out response
	err = exchange(r.Socket, op, q, &out, timeout)
	if err == nil && out.Error != "" {
		switch out.Error {
		case ErrGenerationChanged.Error():
			err = ErrGenerationChanged
		case ErrEnded.Error():
			err = ErrEnded
		default:
			err = errors.New(out.Error)
		}
	}
	if err == nil && out.Record.Ref != family {
		err = errors.New("Herdr runner identity mismatch")
	}
	return out, err
}

func Info(ref string) (Record, error) { r, err := call(ref, "info", request{}); return r.Record, err }

// Replace is conditional on the exact child; a stale or repeated request can
// never terminate a newer generation. Success means process started, not ready.
func Replace(ref string, pid int, dir string, args []string) error {
	g, err := mutationGeneration(ref)
	if err != nil {
		return err
	}
	_, err = call(ref, "replace", request{PID: pid, Generation: g, Dir: dir, Args: args})
	return err
}

func Stop(ref string, pid int) error {
	g, err := mutationGeneration(ref)
	if err != nil {
		return err
	}
	_, err = call(ref, "stop", request{PID: pid, Generation: g})
	if err != nil {
		// Shutdown can close the control connection before its reply is read.
		// Observe only this original family; never retry against a new child.
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
	_, generation, err := ParseRef(ref)
	if err != nil {
		return err
	}
	r, err := Info(ref)
	if err != nil {
		return err
	}
	if generation != 0 && generation != r.Generation {
		return ErrGenerationChanged
	}
	return r.Core.attach()
}
func NameFor(ref string, pid int, name string) error {
	g, err := mutationGeneration(ref)
	if err != nil {
		return err
	}
	_, err = call(ref, "label", request{PID: pid, Generation: g, Label: name})
	return err
}

// Records is only local runner metadata, never a listing of private Herdr
// terminals. Ended/superseded descriptors need no socket query.
func Records() ([]Record, error) {
	dir, err := os.Open(directory())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	es, err := dir.ReadDir(20001)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(es) > 20000 {
		return nil, errors.New("too many Herdr workspace records")
	}
	var out []Record
	var bytes int64
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
		fi, err := e.Info()
		if err != nil {
			return nil, errors.New("Herdr ownership cannot be inspected")
		}
		bytes += fi.Size()
		if bytes > 8*1024*1024 {
			return nil, errors.New("Herdr ownership metadata exceeds discovery limit")
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
		return r + "@" + os.Getenv("AIQ_TERMINAL_GENERATION")
	}
	return os.Getenv("TMUX_PANE")
}

func InfoWithin(ref string, timeout time.Duration) (Record, error) {
	r, err := callFor(ref, "info", request{}, timeout)
	return r.Record, err
}
