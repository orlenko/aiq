package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// `aiq __app <command>` is the native Herdr app's data bridge. Each run reads
// this machine only and writes exactly one complete JSON document (schema 1,
// at most appMaxBytes) to stdout. Handled failures are documents too, with
// exit 0; a nonzero exit means a crash or an aiq without this command. The
// name starts with "__", which config.ValidLauncherName rejects, so an older
// aiq can never run it as a launcher. cmd/aiq/testdata/app holds the contract.

const (
	appSchema   = 1
	appMaxBytes = 4 << 20
	// appEnvelopeRoom is kept free for the envelope around a list's rows.
	appEnvelopeRoom = 64 << 10
	appReadTimeout  = 8 * time.Second
	appWriteTimeout = 5 * time.Second
	appPreviewRunes = 280
	appTextBytes    = 256 << 10
)

// Error codes, a closed set.
const (
	appBusy           = "busy"
	appUnavailable    = "unavailable"
	appNotSetUp       = "not_set_up"
	appTooLarge       = "too_large"
	appInvalid        = "invalid_request"
	appNotFound       = "not_found"
	appConflict       = "conflict"
	appUnknownOutcome = "unknown_outcome"
	appInternal       = "internal"
)

var appCommands = []string{"hello", "asks", "ask-detail", "ask-action", "target", "quotas"}

type appHost struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type appOK struct {
	Schema      int              `json:"schema"`
	OK          bool             `json:"ok"`
	Kind        string           `json:"kind"`
	Aiq         string           `json:"aiq"`
	Host        appHost          `json:"host"`
	GeneratedAt int64            `json:"generated_at_ms"`
	Partial     bool             `json:"partial"`
	Omitted     map[string]int64 `json:"omitted"`
	Data        any              `json:"data"`
}
type appErrorCode struct {
	Code string `json:"code"`
}
type appFail struct {
	Schema      int          `json:"schema"`
	OK          bool         `json:"ok"`
	Kind        string       `json:"kind"`
	Aiq         string       `json:"aiq"`
	Host        appHost      `json:"host"`
	GeneratedAt int64        `json:"generated_at_ms"`
	Error       appErrorCode `json:"error"`
}

// appResult is one command's outcome before the envelope.
type appResult struct {
	data    any
	partial bool
	omitted map[string]int64
	code    string // non-empty: a handled failure
}

func appFailure(code string) appResult { return appResult{code: code} }

// appEnv is everything a bridge command reads. Production values come from
// appProductionEnv; tests substitute invented ones.
type appEnv struct {
	version   string
	now       func() time.Time
	identity  func(context.Context) (string, error)
	hostname  func() (string, error)
	dataDir   string
	asksPath  string
	statePath string
	input     navFactoryInput
	quotas    *navQuotaDeps
	resolve   func(ref string) (herdr.Target, string)
}

func appProductionEnv() appEnv {
	in := navProductionInput()
	return appEnv{version: version, now: time.Now, identity: pool.HostnameContext, hostname: os.Hostname,
		dataDir: paths.DataDir(), asksPath: filepath.Join(paths.DataDir(), asksDBName), statePath: paths.StateDB(),
		input: in, quotas: appProductionQuotas(), resolve: herdr.ResolveTarget}
}

func cmdApp(args []string) error {
	return appRun(appProductionEnv(), args, os.Stdout)
}

// appRun writes one document for args to w. It returns an error only when
// writing fails; every other outcome is in the document.
func appRun(env appEnv, args []string, w io.Writer) error {
	kind := ""
	if len(args) > 0 && appKnown(args[0]) {
		kind = args[0]
	}
	timeout := appReadTimeout
	if kind == "ask-action" {
		timeout = appWriteTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	host := env.host(ctx)
	result := func() (out appResult) {
		defer func() {
			if v := recover(); v != nil {
				out = appFailure(appInternal)
			}
		}()
		if kind == "" {
			return appFailure(appInvalid)
		}
		return env.dispatch(ctx, kind, args[1:], host)
	}()
	doc, err := env.encode(kind, host, result)
	if err != nil {
		doc, _ = env.encode(kind, host, appFailure(appInternal))
	}
	_, err = w.Write(doc)
	return err
}

func appKnown(kind string) bool {
	for _, k := range appCommands {
		if k == kind {
			return true
		}
	}
	return false
}

func (env appEnv) dispatch(ctx context.Context, kind string, args []string, host appHost) appResult {
	switch kind {
	case "hello":
		if _, ok := appFlags(args, nil); !ok {
			return appFailure(appInvalid)
		}
		return env.hello()
	case "asks":
		if _, ok := appFlags(args, nil); !ok {
			return appFailure(appInvalid)
		}
		return env.asks(ctx)
	case "ask-detail":
		f, ok := appFlags(args, map[string]*regexp.Regexp{"id": appIDPattern})
		if !ok || f["id"] == "" {
			return appFailure(appInvalid)
		}
		return env.askDetail(ctx, f["id"])
	case "ask-action":
		f, ok := appFlags(args, map[string]*regexp.Regexp{"id": appIDPattern, "revision": appRevisionPattern, "action": appActionPattern, "host": appHostPattern})
		if !ok || len(f) != 4 {
			return appFailure(appInvalid)
		}
		if host.ID == "" || f["host"] != host.ID {
			return appFailure(appInvalid) // asked of another installation
		}
		return env.askAction(ctx, f["id"], f["revision"], f["action"])
	case "target":
		f, ok := appFlags(args, map[string]*regexp.Regexp{"ask": appIDPattern, "revision": appRevisionPattern, "provider": appProviderPattern, "conversation": appConversationPattern})
		if !ok {
			return appFailure(appInvalid)
		}
		return env.target(ctx, f)
	case "quotas":
		if _, ok := appFlags(args, nil); !ok {
			return appFailure(appInvalid)
		}
		return env.quotasData(ctx)
	}
	return appFailure(appInvalid)
}

var (
	appIDPattern           = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
	appRevisionPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	appActionPattern       = regexp.MustCompile(`^(dismiss|restore)$`)
	appHostPattern         = regexp.MustCompile(`^[0-9a-f]{32}$`)
	appProviderPattern     = regexp.MustCompile(`^(claude|codex|agy)$`)
	appConversationPattern = regexp.MustCompile(`^[0-9A-Za-z-]{1,128}$`)
)

// appFlags admits only `--name value` pairs named in allowed, each once, each
// matching its pattern. No positional arguments, no `--name=value`.
func appFlags(args []string, allowed map[string]*regexp.Regexp) (map[string]string, bool) {
	out := map[string]string{}
	if len(args)%2 != 0 {
		return nil, false
	}
	for i := 0; i < len(args); i += 2 {
		name, value := args[i], args[i+1]
		if len(name) < 3 || name[:2] != "--" {
			return nil, false
		}
		name = name[2:]
		pattern := allowed[name]
		if pattern == nil || !pattern.MatchString(value) {
			return nil, false
		}
		if _, dup := out[name]; dup {
			return nil, false
		}
		out[name] = value
	}
	return out, true
}

// host is this installation's source: a hash of the machine identity (machine
// ID or platform UUID first) and the canonical aiq data directory, so two
// installations on one machine never alias. An unavailable identity gives an
// empty ID, which no action accepts.
func (env appEnv) host(ctx context.Context) appHost {
	out := appHost{}
	if env.hostname != nil {
		if name, err := env.hostname(); err == nil && len(name) <= 255 {
			out.Name = name
		}
	}
	if env.identity == nil || env.dataDir == "" {
		return out
	}
	id, err := env.identity(ctx)
	if err != nil || id == "" {
		return out
	}
	sum := sha256.Sum256([]byte("aiq-app-source-v1\x00" + id + "\x00" + canonicalDir(env.dataDir)))
	out.ID = hex.EncodeToString(sum[:16])
	return out
}

func (env appEnv) encode(kind string, host appHost, r appResult) ([]byte, error) {
	at := env.now().UnixMilli()
	var v any
	if r.code != "" {
		v = appFail{Schema: appSchema, Kind: kind, Aiq: env.version, Host: host, GeneratedAt: at, Error: appErrorCode{r.code}}
	} else {
		omitted := r.omitted
		if omitted == nil {
			omitted = map[string]int64{}
		}
		v = appOK{Schema: appSchema, OK: true, Kind: kind, Aiq: env.version, Host: host, GeneratedAt: at, Partial: r.partial, Omitted: omitted, Data: r.data}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if buf.Len() > appMaxBytes {
		if r.code != "" {
			return nil, errors.New("error document too large")
		}
		return env.encode(kind, host, appFailure(appTooLarge))
	}
	return buf.Bytes(), nil
}

// appJSONSize is v's encoded size, as encode writes it, without the newline.
func appJSONSize(v any) (int, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return 0, err
	}
	return buf.Len() - 1, nil
}

type appSetup struct {
	Asks   string `json:"asks"`
	Quotas string `json:"quotas"`
}
type appSchemas struct {
	Min int `json:"min"`
	Max int `json:"max"`
}
type appHello struct {
	Schemas  appSchemas `json:"schemas"`
	Setup    appSetup   `json:"setup"`
	Commands []string   `json:"commands"`
}

func (env appEnv) hello() appResult {
	var h appHello
	h.Schemas.Min, h.Schemas.Max = appSchema, appSchema
	h.Setup = appSetup{Asks: appSetupState(env.asksPath), Quotas: appSetupState(env.statePath)}
	h.Commands = append([]string{}, appCommands...)
	return appResult{data: h}
}

func appSetupState(path string) string {
	if path == "" {
		return "not_set_up"
	}
	if _, err := os.Lstat(path); err != nil {
		return "not_set_up"
	}
	return "ready"
}

// appErrorFor maps a store error to a fixed code.
func appErrorFor(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil, errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return appBusy
	case errors.Is(err, state.ErrAsksSnapshotTooLarge), errors.Is(err, state.ErrNavigatorSnapshotTooLarge), errors.Is(err, state.ErrQuotaSnapshotTooLarge):
		return appTooLarge
	}
	return appUnavailable
}

func appMillis(seconds int64) int64 {
	if seconds <= 0 || seconds > navConversationSafeInteger/1000 {
		return 0
	}
	return seconds * 1000
}

func appIDString(id int64) string { return strconv.FormatInt(id, 10) }
