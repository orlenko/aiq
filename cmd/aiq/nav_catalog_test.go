package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func catalogTranscript(t *testing.T, root, cwd, id string, at time.Time, worker bool) {
	t.Helper()
	record := map[string]any{"type": "user", "sessionId": id, "cwd": cwd, "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]string{"role": "user", "content": "Invented catalog prompt"}}
	if worker {
		record["entrypoint"] = "sdk-fixture"
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	goldenWrite(t, filepath.Join(root, claudeNonAlnum.ReplaceAllString(cwd, "-"), id+".jsonl"), string(data))
}

func TestNavCatalogIncludesDiskAndLaunchDirectoriesWithoutWorkerDefault(t *testing.T) {
	x := newNavAppFixture(t, 0, "Invented answer")
	worker := filepath.Join(x.root, "worker-only")
	catalogTranscript(t, x.roots.ClaudeProjects[0], worker, "00000002-0000-4000-8000-000000000002", x.at.Add(24*time.Hour), true)
	in := x.input(nil)
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return &state.NavigatorSnapshot{Launches: []state.NavigatorLaunch{
			{Provider: "claude", Mode: state.ModeInteractive, Cwd: "/invented/metadata-only", StartedMS: 1000, AccountID: "INVENTED-NOT-PUBLIC"},
		}}, nil
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	w := factoryGet(t, f, "/api/nav/directories?catalog=1")
	var body navCatalogBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Source != "session index and launch metadata" || body.Partial || len(body.Directories) != 3 {
		t.Fatal("full catalog missing", w.Code, w.Body.String())
	}
	if body.DefaultDir != x.work || body.DefaultReason != "most recent activity" {
		t.Fatal("newer worker directory became default", body.DefaultDir)
	}
	if body.Directories[0].Path != worker || body.Directories[0].Workers != 1 || body.Directories[0].Sessions != 0 {
		t.Fatal("worker facts lost")
	}
	if strings.Contains(w.Body.String(), "INVENTED-NOT-PUBLIC") || strings.Contains(w.Body.String(), ".jsonl") {
		t.Fatal("catalog leaked non-directory data")
	}
	// Existing metadata-only clients retain their original closed response.
	legacy := factoryGet(t, f, "/api/nav/directories")
	var old navDirectoriesBody
	if legacy.Code != 200 || json.Unmarshal(legacy.Body.Bytes(), &old) != nil || old.Source != "launch metadata" || len(old.Directories) != 1 {
		t.Fatal("legacy metadata route changed")
	}
}

func TestNavCatalogMissingMetadataAndWholeAdmission(t *testing.T) {
	x := newNavAppFixture(t, 0, "Invented answer")
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	w := factoryGet(t, f, "/api/nav/directories?catalog=1")
	var body navCatalogBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || !body.Partial || body.MetadataKnown || body.Source != "session index" || body.DefaultDir != x.work {
		t.Fatal("missing metadata fabricated complete coverage", w.Code, w.Body.String())
	}
	f.policy.sessions.rowBytes = 512
	w = factoryGet(t, f, "/api/nav/directories?catalog=1")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "list_too_large") || strings.Contains(w.Body.String(), "directories") {
		t.Fatal("summary admission produced a partial success")
	}
	f.policy = navProductionPolicy()
	f.input.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return nil, state.ErrNavigatorSnapshotTooLarge
	}
	if w := factoryGet(t, f, "/api/nav/directories?catalog=1"); w.Code != 503 {
		t.Fatal("metadata admission was bypassed by fallback")
	}
}

func TestNavAllSessionsIsExplicitAndUsesExistingRowBounds(t *testing.T) {
	x := newNavAppFixture(t, 0, "Invented answer")
	other := filepath.Join(x.root, "other-work")
	catalogTranscript(t, x.roots.ClaudeProjects[0], other, "00000002-0000-4000-8000-000000000002", x.at.Add(time.Hour), false)
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/nav/sessions", "/api/nav/sessions?all=0", "/api/nav/sessions?all=1&dir=", "/api/nav/sessions?all=true", "/api/nav/sessions?all=1&all=1"} {
		if w := factoryGet(t, f, path); w.Code != 400 {
			t.Fatal("implicit/ambiguous all-scope request accepted", path, w.Code)
		}
	}
	w := factoryGet(t, f, "/api/nav/sessions?all=1&workers=0")
	var body navSessionsBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Dir != "" || len(body.Sessions) != 2 || body.Partial {
		t.Fatal("explicit all-scope failed", w.Code, w.Body.String())
	}
	if body.Sessions[0].Cwd != other || body.Sessions[1].Cwd != x.work {
		t.Fatal("all-scope did not preserve recorded directories")
	}
}

func TestNavCatalogReportsUnassignedSessionsWithoutLosingAllScopeAccess(t *testing.T) {
	x := newNavAppFixture(t, 0, "Invented answer")
	agy := filepath.Join(x.root, "agy-app")
	x.roots.AgyAppData = []string{agy}
	id := "00000003-0000-4000-8000-000000000003"
	data, err := json.Marshal(map[string]string{"type": "USER_INPUT", "source": "USER_EXPLICIT", "status": "DONE", "created_at": x.at.Format(time.RFC3339Nano), "content": "Invented unassigned prompt"})
	if err != nil {
		t.Fatal(err)
	}
	goldenWrite(t, transcript.AgyTranscript(agy, id), string(data))
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	w := factoryGet(t, f, "/api/nav/directories?catalog=1")
	var body navCatalogBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.UnknownDirectories != 1 || !body.Partial {
		t.Fatal("unassigned directory was fabricated", w.Code, w.Body.String())
	}
	w = factoryGet(t, f, "/api/nav/sessions?all=1&workers=0")
	var sessions navSessionsBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &sessions) != nil || len(sessions.Sessions) != 2 {
		t.Fatal("unassigned session disappeared from All Sessions")
	}
	found := false
	for _, row := range sessions.Sessions {
		if row.ID == id {
			found = row.Cwd == ""
		}
	}
	if !found {
		t.Fatal("unassigned session acquired a fabricated cwd")
	}
}
