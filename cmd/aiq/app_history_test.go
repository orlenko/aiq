package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestAppHistoryRecentBeforeParseAndStablePaging(t *testing.T) {
	root := t.TempDir()
	candidates := []transcript.Candidate{}
	for i := 0; i < 7; i++ {
		path := filepath.Join(root, fmt.Sprintf("%d.jsonl", i))
		if err := os.WriteFile(path, []byte("invented"), 0600); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(100+int64(i), 0)
		os.Chtimes(path, at, at)
		candidates = append(candidates, transcript.Candidate{Provider: "claude", Path: path})
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 4; page++ {
		rows, total, unreadable, err := appHistoryRecent(context.Background(), candidates, 2, cursor)
		if err != nil || unreadable != 0 {
			t.Fatal(err, unreadable)
		}
		if total != 7-len(seen) {
			t.Fatal("wrong remaining count", total, len(seen))
		}
		for _, r := range rows {
			if seen[r.key] {
				t.Fatal("page repeated a candidate")
			}
			seen[r.key] = true
			cursor = fmt.Sprintf("%d.%s", r.at, r.key)
			if strings.Contains(cursor, root) {
				t.Fatal("cursor disclosed source path")
			}
		}
		if page == 0 && filepath.Base(rows[0].candidate.Path) != "6.jsonl" {
			t.Fatal("candidates were not limited by recency before parsing")
		}
	}
	if len(seen) != 7 {
		t.Fatal("lost candidates", len(seen))
	}
}

func TestAppHistorySessionsColdWindowProgress(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	roots := transcript.Roots{ClaudeProjects: []string{projects}}
	for i := 0; i < 5; i++ {
		id := appHistoryFixtureID(4, i)
		path := filepath.Join(projects, "invented", id+".jsonl")
		appHistoryWriteLines(t, path,
			fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/invented/work","timestamp":"2031-07-01T10:00:00Z","message":{"content":"Invented request."}}`, id),
			fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":"2031-07-01T10:00:01Z","message":{"content":[{"type":"text","text":"Invented reply."}]}}`, id))
		at := time.Unix(100+int64(i), 0)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	d := appHistoryDeps{input: navFactoryInput{now: time.Now, asksPath: filepath.Join(root, "absent.db")}, cache: transcript.NewCache(), source: transcript.NewIndex(roots)}
	first, err := d.run(ctx, roots, "sessions", []string{"--limit", "2"})
	if err != nil {
		t.Fatal(err)
	}
	data := first.Data.(map[string]any)
	rows := data["rows"].([]map[string]any)
	if len(rows) != 2 || first.Omitted["older"] != 3 {
		t.Fatal("bad bounded recent window", len(rows), first.Omitted)
	}
	cursor := data["window"].(map[string]any)["cursor"].(string)
	second, err := d.run(ctx, roots, "sessions", []string{"--limit", "2", "--before", cursor})
	if err != nil {
		t.Fatal(err)
	}
	next := second.Data.(map[string]any)["rows"].([]map[string]any)
	if len(next) != 2 || next[0]["id"] == rows[0]["id"] {
		t.Fatal("older cursor did not advance")
	}
}

func TestAppHistoryWindowKeepsAnchorAndPagesBothDirections(t *testing.T) {
	at := time.Unix(123, 0)
	s := &transcript.Session{Provider: "claude", ID: appHistoryFixtureID(5, 1)}
	for i := 0; i < 30; i++ {
		s.Turns = append(s.Turns, transcript.Turn{Prompt: strings.Repeat("x", 256<<10), Started: at.Add(time.Duration(i) * time.Second), Reply: "Invented answer", ReplyAt: at.Add(time.Duration(i)*time.Second + time.Millisecond)})
	}
	ctx := context.Background()
	data, partial, err := appHistoryWindow(ctx, s, s.Turns[15].Started.UnixMilli(), "", "")
	if err != nil || !partial {
		t.Fatal(err, partial)
	}
	rows := data["messages"].([]appHistoryMessage)
	found := false
	for _, r := range rows {
		found = found || r.Turn == 15
	}
	if !found {
		t.Fatal("output budget removed the anchor")
	}
	cursor := data["cursor"].(map[string]string)
	if cursor["before"] == "" || cursor["after"] == "" {
		t.Fatal("missing page cursors")
	}
	older, _, err := appHistoryWindow(ctx, s, 0, cursor["before"], "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rows {
		for _, b := range older["messages"].([]appHistoryMessage) {
			if a.Key == b.Key {
				t.Fatal("page overlap")
			}
		}
	}
	if _, _, err := appHistoryWindow(ctx, s, 0, "missing", ""); err != appHistoryFailure("conflict") {
		t.Fatal("changed transcript cursor must be explicit", err)
	}
	b, err := json.Marshal(data)
	if err != nil || len(b) > 4<<20 {
		t.Fatal("window exceeded transport", len(b), err)
	}
}

func TestAppHistoryAskContextLocatesOriginalMessage(t *testing.T) {
	root := t.TempDir()
	id := appHistoryFixtureID(6, 1)
	path := filepath.Join(root, "projects", "invented", id+".jsonl")
	quote := "Should I keep the invented fixture or regenerate it?"
	lines := []string{`{"type":"user","sessionId":"` + id + `","cwd":"/invented/work","timestamp":"2026-01-01T12:00:00Z","message":{"content":"Prepare the invented fixture."}}`}
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"assistant","timestamp":"2026-01-01T12:00:01Z","message":{"id":"invented-%d","content":[{"type":"text","text":"Progress %d on the invented fixture."}]}}`, i, i))
	}
	lines = append(lines, `{"type":"assistant","timestamp":"2026-01-01T12:00:01Z","message":{"content":[{"type":"text","text":"`+quote+`"}]}}`)
	appHistoryWriteLines(t, path, lines...)
	roots := transcript.Roots{ClaudeProjects: []string{filepath.Join(root, "projects")}}
	ask := state.Ask{ID: 17, Provider: "claude", SessionID: id, RaisedAt: time.Date(2026, 1, 1, 12, 0, 2, 0, time.UTC).Unix(), FirstQuote: quote, LastQuote: quote, State: state.AskOpen}
	d := appHistoryDeps{cache: transcript.NewCache(), source: transcript.NewIndex(roots), readAsk: func(_ context.Context, id int64, revision string) (state.Ask, error) {
		if id != 17 || revision != state.AskActionRevision(ask) {
			t.Fatal("context did not bind row revision")
		}
		return ask, nil
	}}
	out, err := d.run(context.Background(), roots, "ask-context", []string{"--id", "17", "--revision", state.AskActionRevision(ask)})
	if err != nil {
		t.Fatal(err)
	}
	data := out.Data.(map[string]any)
	if data["placement"].(map[string]any)["confidence"] != "time+text" {
		t.Fatal("unique quote was not located")
	}
	found := false
	for _, m := range data["messages"].([]appHistoryMessage) {
		if m.Anchor && strings.Contains(m.Text, quote) {
			found = true
		}
	}
	if !found {
		t.Fatal("matching message was not marked for scroll and highlight")
	}
}

func TestAppHistoryUnicodeBoundaryAndDuplicateAnswer(t *testing.T) {
	text, cut := appHistoryText("ééé", 5)
	if text != "éé" || !cut {
		t.Fatal(text, cut)
	}
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "Invented question", Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "First answer"}, {Kind: transcript.ItemAnswer, Text: "First answer", Promotes: true}, {Kind: transcript.ItemAnswer, Text: "Corrected answer", Supersedes: true}}}}}
	rows, err := appHistoryMessages(context.Background(), s)
	if err != nil || len(rows) != 2 || rows[1].Text != "Corrected answer" {
		t.Fatal(rows, err)
	}
}

func TestAppHistoryWindowCapsMessageCount(t *testing.T) {
	s := &transcript.Session{Provider: "claude", ID: appHistoryFixtureID(7, 1)}
	for i := 0; i < 5000; i++ {
		s.Turns = append(s.Turns, transcript.Turn{Prompt: "Continue", Reply: "Done"})
	}
	window, partial, err := appHistoryWindow(context.Background(), s, 0, "", "")
	if err != nil || !partial {
		t.Fatal(err, partial)
	}
	if len(window["messages"].([]appHistoryMessage)) != 40 {
		t.Fatal("native view would render an unbounded number of messages")
	}
}

func TestAppHistoryLongMessageExcerptKeepsMatchingQuote(t *testing.T) {
	quote := "Should I keep the invented fixture or regenerate it?"
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "Invented work", Reply: strings.Repeat("Earlier progress. ", 30000) + quote}}}
	rows, err := appHistoryMessagesWithQuote(context.Background(), s, []string{quote})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows[1].Truncated || !strings.Contains(rows[1].Text, quote) || len(rows[1].Text) > 256<<10 {
		t.Fatal("excerpt lost the ask at the end of a long message")
	}
}

func TestAppHistoryPreservesWhoSuppliedAPrompt(t *testing.T) {
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "Invented peer message", Source: transcript.Peer}, {Prompt: "Invented notice", Source: transcript.Notice}, {Prompt: "Invented takeover", Source: transcript.Aiq}, {Prompt: "Invented human request", Source: transcript.Human}}}
	rows, err := appHistoryMessages(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for i, source := range []string{"peer", "notice", "aiq"} {
		if rows[i].Role == "user" || rows[i].Source != source {
			t.Fatal("attributed external text to the person", rows[i])
		}
	}
	if rows[3].Role != "user" || rows[3].Source != "" {
		t.Fatal("lost human attribution", rows[3])
	}
}

func TestAppHistoryRejectsRepeatedEmptyFlag(t *testing.T) {
	_, err := (appHistoryDeps{}).run(context.Background(), transcript.Roots{}, "sessions", []string{"--limit", "", "--limit", "1"})
	if err != appHistoryFailure("invalid_request") {
		t.Fatal("repeated flag was admitted", err)
	}
}

func TestAppHistoryQuotePlacementIgnoresFormattingButRefusesAmbiguity(t *testing.T) {
	at := time.Unix(100, 0)
	quote := "Should I remove the invented fixture and run the migration now?"
	s := &transcript.Session{Turns: []transcript.Turn{{Started: at, Reply: "Should I remove the **invented fixture**\nand run the migration now?"}}}
	ask := state.Ask{RaisedAt: 200, FirstQuote: quote, LastQuote: quote}
	anchor, err := appHistoryPlaceAsk(context.Background(), ask, s)
	if err != nil || anchor == nil || *anchor != at.UnixMilli() {
		t.Fatal("formatting hid a unique original question", anchor, err)
	}
	s.Turns = append(s.Turns, transcript.Turn{Started: at.Add(time.Second), Reply: s.Turns[0].Reply})
	anchor, err = appHistoryPlaceAsk(context.Background(), ask, s)
	if err != nil || anchor != nil {
		t.Fatal("ambiguous quote was assigned to a turn", anchor, err)
	}
}

func TestAppHistorySlowCandidateDoesNotTrapOlderPaging(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	newer := filepath.Join(projects, "invented", appHistoryFixtureID(8, 1)+".jsonl")
	older := filepath.Join(projects, "invented", appHistoryFixtureID(8, 2)+".jsonl")
	appHistoryWriteLines(t, newer, "{}")
	appHistoryWriteLines(t, older, "{}")
	os.Chtimes(newer, time.Unix(200, 0), time.Unix(200, 0))
	os.Chtimes(older, time.Unix(100, 0), time.Unix(100, 0))
	roots := transcript.Roots{ClaudeProjects: []string{projects}}
	d := appHistoryDeps{input: navFactoryInput{now: time.Now}, source: transcript.NewIndex(roots), summary: func(_ context.Context, path string) (transcript.Summary, error) {
		if path == newer {
			return transcript.Summary{}, transcript.ErrBudget
		}
		return transcript.Summary{Provider: "claude", ID: appHistoryFixtureID(8, 2), Label: "Invented older session"}, nil
	}}
	out, err := d.run(context.Background(), roots, "sessions", []string{"--limit", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Partial || out.Omitted["slow"] != 1 {
		t.Fatal("slow input became an empty complete result", out)
	}
	cursor := out.Data.(map[string]any)["window"].(map[string]any)["cursor"].(string)
	if cursor == "" {
		t.Fatal("slow newest candidate trapped the one-shot client")
	}
	next, err := d.run(context.Background(), roots, "sessions", []string{"--limit", "1", "--before", cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Data.(map[string]any)["rows"].([]map[string]any)) != 1 {
		t.Fatal("could not advance past a slow candidate")
	}
}

func FuzzAppHistoryCursor(f *testing.F) {
	for _, s := range []string{"", "0.00000000000000000000000000000000", "../../x", "12.bad", "-1.ffffffffffffffffffffffffffffffff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1024 {
			return
		}
		_, _, _, _ = appHistoryRecent(context.Background(), nil, 100, s)
	})
}

func TestAppGoldenHistory(t *testing.T) {
	env, x := appTestEnv(t)
	window, code := env.appReadAsks(context.Background(), appOne("17"))
	if code != "" || len(window.Items) != 1 {
		t.Fatal("invented ask unavailable", code)
	}
	ask := window.Items[0]
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	path := filepath.Join(projects, "invented", fullIDFixtureID+".jsonl")
	appHistoryWriteLines(t, path, `{"type":"user","sessionId":"`+fullIDFixtureID+`","cwd":"/invented/work","timestamp":"2031-07-01T10:58:00Z","message":{"content":"Prepare the invented branch."}}`,
		`{"type":"assistant","timestamp":"2031-07-01T10:59:30Z","message":{"content":[{"type":"text","text":`+appHistoryJSONString(ask.LastQuote)+`}]}}`)
	env.input.roots = func(context.Context) (transcript.Roots, error) {
		return transcript.Roots{ClaudeProjects: []string{projects}}, nil
	}
	env.input.metadata = nil
	appGolden(t, "sessions", env, x, "sessions")
	appGolden(t, "conversation", env, x, "conversation", "--provider", "claude", "--id", fullIDFixtureID)
	appGolden(t, "ask-context", env, x, "ask-context", "--id", "17", "--revision", state.AskActionRevision(ask))
}

func appHistoryFixtureID(kind, i int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", kind, i)
}
func appHistoryWriteLines(t testing.TB, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func appHistoryJSONString(s string) string { b, _ := json.Marshal(s); return string(b) }
