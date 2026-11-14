package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestNavDirectoriesClosedCatalogAndLimits(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	in := x.input(nil)
	calls := 0
	in.metadata = func(ctx context.Context, limits state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		calls++
		if limits.Rows != 262144 || limits.TextBytes != 16<<20 {
			t.Fatal("different admission policy")
		}
		return &state.NavigatorSnapshot{Launches: []state.NavigatorLaunch{
			{Provider: "claude", Cwd: "/invented/B", Mode: state.ModeLong, StartedMS: 2000, AccountID: "INVENTED-ACCOUNT-NOT-IN-DTO"},
			{Provider: "codex", Cwd: "/invented/A", Mode: state.ModeInteractive, StartedMS: 2000},
			{Provider: "agy", Cwd: "/invented/B", Mode: state.ModeInteractive, StartedMS: 1000},
			{Provider: "claude", Cwd: "/invented/worker", Mode: state.ModeWorker, StartedMS: 9000},
		}}, ctx.Err()
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("constructor read metadata")
	}
	w := factoryGet(t, f, "/api/nav/directories")
	var body navDirectoriesBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Source != "launch metadata" || len(body.Directories) != 2 || body.Directories[0].Path != "/invented/A" || body.Directories[1].UpdatedMS != 2000 {
		t.Fatalf("catalog: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "INVENTED-ACCOUNT") || strings.Contains(w.Body.String(), "worker") {
		t.Fatal("excluded facts exposed")
	}
	for _, target := range []string{"/api/nav/directories?", "/api/nav/directories?arbitrary=1"} {
		if w := factoryGet(t, f, target); w.Code == 200 {
			t.Fatal("nonempty/noncanonical query admitted")
		}
	}
	f.policy.sessions.bodyBytes = 64
	if w := factoryGet(t, f, "/api/nav/directories"); w.Code != 503 || !strings.Contains(w.Body.String(), "list_too_large") {
		t.Fatal("oversize successful partial result")
	}
	f.policy.sessions.bodyBytes = 16 << 20
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return nil, state.ErrNavigatorSnapshotTooLarge
	}
	f.input = in
	if w := factoryGet(t, f, "/api/nav/directories"); w.Code != 503 || !strings.Contains(w.Body.String(), "list_too_large") {
		t.Fatal("reader admission classification")
	}
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return nil, errors.New("INVENTED-PRIVATE-DRIVER")
	}
	f.input = in
	if w := factoryGet(t, f, "/api/nav/directories"); w.Code != 503 || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatal("reader failure disclosure")
	}
}

type navDirectoryWriter struct {
	header http.Header
	status int
	write  func()
}

func (w *navDirectoryWriter) Header() http.Header         { return w.header }
func (w *navDirectoryWriter) WriteHeader(status int)      { w.status = status }
func (w *navDirectoryWriter) Write(p []byte) (int, error) { w.write(); return len(p), nil }
func TestNavDirectoriesShareSessionSlotThroughWrite(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	in := x.input(nil)
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return &state.NavigatorSnapshot{}, nil
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.sessions.(*navSessionsHandler).slots != f.sessionSlots {
		t.Fatal("list admissions differ")
	}
	w := &navDirectoryWriter{header: make(http.Header), write: func() {
		if len(f.sessionSlots) != 1 {
			t.Fatal("released builder before write")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := httptest.NewRequest("GET", "/api/nav/sessions?dir=/invented", nil).WithContext(ctx)
		out := httptest.NewRecorder()
		b.sessions.ServeHTTP(out, request)
		if out.Code != 503 {
			t.Fatal("cancelled list read overlapped")
		}
	}}
	f.api.ServeHTTP(w, httptest.NewRequest("GET", "/api/nav/directories", nil))
	if w.status != 200 || len(f.sessionSlots) != 0 {
		t.Fatal("admission not released")
	}
	f.sessionSlots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	out := httptest.NewRecorder()
	f.api.ServeHTTP(out, httptest.NewRequest("GET", "/api/nav/directories", nil).WithContext(ctx))
	<-f.sessionSlots
	if out.Code != 503 {
		t.Fatal("busy slot did not honor cancellation")
	}
}

func TestNavDirectoriesProductionMissingAndReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	read := navProductionMetadata(path, func() (string, error) { return "fixture-host", nil })
	if v, err := read(context.Background(), state.NavigatorSnapshotLimits{Rows: 20, TextBytes: 4096}); v != nil || err != state.ErrNavigatorSnapshotUnavailable {
		t.Fatal("missing DB fabricated empty snapshot")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created state DB")
	}
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.AddLaunch(state.Launch{Hostname: "fixture-host", Provider: "claude", Cwd: "/invented/catalog", Mode: state.ModeInteractive, StartedAt: time.Unix(100, 0), Args: []string{"INVENTED-HIDDEN"}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := read(context.Background(), state.NavigatorSnapshotLimits{Rows: 20, TextBytes: 4096})
	if err != nil || len(v.Launches) != 1 || v.Launches[0].Cwd != "/invented/catalog" {
		t.Fatalf("production read-only: %#v %v", v, err)
	}
}
