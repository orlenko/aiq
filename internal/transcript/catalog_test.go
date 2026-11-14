package transcript

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestCatalogAgyDirectoryAssociationIsKnownOnlyWhenUnique(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "unique", true: "ambiguous"}[ambiguous], func(t *testing.T) {
			root := t.TempDir()
			id := "00000001-0000-4000-8000-000000000001"
			writeLines(t, AgyTranscript(root, id), agyFixture()...)
			first, _ := json.Marshal(map[string]any{"display": "invented", "timestamp": 1, "workspace": "/invented/A", "conversationId": id})
			lines := []string{string(first)}
			if ambiguous {
				second, _ := json.Marshal(map[string]any{"display": "invented", "timestamp": 2, "workspace": "/invented/B", "conversationId": id})
				lines = append(lines, string(second))
			}
			writeLines(t, filepath.Join(root, "history.jsonl"), lines...)
			index := NewLiveIndex(Roots{AgyAppData: []string{root}})
			rows, err := ListAll(context.Background(), index, ParseSummary)
			if err != nil || len(rows.Rows) != 1 {
				t.Fatal("AGY catalog failed", rows, err)
			}
			want := "/invented/A"
			if ambiguous {
				want = ""
			}
			if rows.Rows[0].Cwd != want {
				t.Fatal("ambiguous directory association guessed", rows.Rows[0].Cwd)
			}
		})
	}
}

func TestCatalogUnassignedAgyTranscriptsStayReadable(t *testing.T) {
	for _, workspace := range []string{"", "relative-directory"} {
		t.Run(workspace, func(t *testing.T) {
			root := t.TempDir()
			id := "00000001-0000-4000-8000-000000000001"
			writeLines(t, AgyTranscript(root, id), agyFixture()...)
			if workspace != "" {
				row, _ := json.Marshal(map[string]any{"display": "invented", "timestamp": 1, "workspace": workspace, "conversationId": id})
				writeLines(t, filepath.Join(root, "history.jsonl"), string(row))
			}
			rows, err := ListAll(context.Background(), NewLiveIndex(Roots{AgyAppData: []string{root}}), ParseSummary)
			if err != nil || len(rows.Rows) != 1 || rows.Rows[0].ID != id || rows.Rows[0].Cwd != "" {
				t.Fatal("unassigned AGY session was hidden or assigned the process directory", rows, err)
			}
		})
	}
}
