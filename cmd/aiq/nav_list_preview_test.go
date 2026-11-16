package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/transcript"
)

type navPreviewAllSource struct{ navTestSource }

func (s navPreviewAllSource) AllCandidates(ctx context.Context) ([]transcript.Candidate, error) {
	return s.Candidates(ctx, "")
}

func TestNavListLargeOpeningPromptsRemainDisplayPreviews(t *testing.T) {
	// A legitimate catalog exceeds admission with full prompts, but its display
	// previews and the capped result fit the existing production allowance.
	const count = 1500
	sourceLabel := strings.Repeat("invented opening prompt ", 1024)
	rows := make([]transcript.Summary, count)
	for i := range rows {
		rows[i] = transcript.Summary{Provider: "claude", ID: fmt.Sprintf("%08x-1111-4222-8333-000000000001", i+1), Label: sourceLabel, Updated: time.Unix(int64(i), 0)}
	}
	h := navTestHandler(t, rows, func(d *navSessionsDeps, l *navSessionsLimits) {
		d.source = navPreviewAllSource{d.source.(navTestSource)}
		*l = navProductionPolicy().sessions
	})
	out := navTestDecode(t, navTestServe(h, httptest.NewRequest("GET", "/api/nav/sessions?all=1", nil)))
	got := navTestRows(t, out)
	if len(got) != transcript.ListRows || out["partial"] != true || out["scanned"] != float64(count) {
		t.Fatal("large catalog lost its bounded results")
	}
	if got[0].(map[string]any)["id"] != rows[count-1].ID {
		t.Fatal("preview changed ordering or identity")
	}
	for _, v := range got {
		label := v.(map[string]any)["label"].(string)
		if len(label) > 512 || !strings.HasSuffix(label, "…") {
			t.Fatal("full prompt escaped preview admission")
		}
	}
	if rows[0].Label != sourceLabel || len(rows[0].Label) <= 512 {
		t.Fatal("shared source summary was modified")
	}
}

func TestNavListPreviewUTF8AndLateTitles(t *testing.T) {
	for _, text := range []string{strings.Repeat("界", 300), strings.Repeat("🌳", 300), strings.Repeat("a", 509) + "界界", strings.Repeat("a", 512)} {
		got := navListPreview(text)
		if !utf8.ValidString(got) || len(got) > 512 {
			t.Fatal("preview split a codepoint or exceeded its limit")
		}
		if len(text) > 512 && !strings.HasSuffix(got, "…") {
			t.Fatal("preview did not identify truncation")
		}
	}
	full := strings.Repeat("invented renamed title ", 1024)
	h := navTestHandler(t, []transcript.Summary{{Provider: "codex", ID: navTestID}}, func(d *navSessionsDeps, l *navSessionsLimits) {
		s := d.source.(navTestSource)
		s.names = func(context.Context) map[string]string { return map[string]string{navTestID: full} }
		d.source = s
		l.rowBytes = 4096
	})
	out := navTestDecode(t, navTestServe(h, navTestRequest("/fixture")))
	row := navTestRows(t, out)[0].(map[string]any)
	if row["label"] != navListPreview(full) || row["title"] != navListPreview(full) || row["id"] != navTestID {
		t.Fatal("late title bypassed preview or changed identity")
	}
}
