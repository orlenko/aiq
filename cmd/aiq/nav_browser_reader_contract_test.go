package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
)

func TestNavBrowserConversationWireContract(t *testing.T) {
	raw, err := os.ReadFile("../../internal/daemon/ui/auth_reader_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Query struct {
			Provider, ID string
			Last         any
			Markup       bool
			Anchor       any
		}
		Path string
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	valid := 0
	for _, row := range rows {
		if row.Path == "" {
			continue
		} // JS asserts refusal-before-fetch for these.
		valid++
		request := httptest.NewRequest("GET", row.Path, nil)
		q, _, ok := navDetailDecode(request)
		wantAll := row.Query.Last == "all"
		wantLast := 0
		if !wantAll {
			wantLast = int(row.Query.Last.(float64))
		}
		if !ok || q.provider != row.Query.Provider || q.id != row.Query.ID || q.last != wantLast || q.all != wantAll || q.markup != row.Query.Markup || q.anchor != int64(row.Query.Anchor.(float64)) {
			t.Fatal("browser literal does not match the actual conversation decoder")
		}
		if q.anchor == 0 && request.URL.Query().Has("at") {
			t.Fatal("unanchored browser request emitted at")
		}
	}
	if len(rows) != 10 || valid != 4 {
		t.Fatal("browser wire contract fixture incomplete")
	}
}
