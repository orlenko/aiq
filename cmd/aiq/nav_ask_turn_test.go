package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestNavAskTurnConservativePromptPlacement(t *testing.T) {
	const quote = "Which invented option should be used?"
	at := time.Unix(100, 123000000)
	for _, row := range []struct {
		mode string
		want bool
	}{
		{"reply", true}, {"narration", true}, {"earlier", true}, {"unplaced", false}, {"ambiguous", false}, {"unknown-start", false}, {"before-start", false}, {"short", false}, {"user-text-only", false}, {"two-quotes-two-turns", false},
	} {
		t.Run(row.mode, func(t *testing.T) {
			a := state.AskPlacement{RaisedAt: 101, FirstQuote: quote, LatestQuote: quote}
			s := &transcript.Session{Turns: []transcript.Turn{{Started: at, Reply: quote}}}
			switch row.mode {
			case "narration":
				s.Turns[0].Reply = ""
				s.Turns[0].Items = []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: quote}}
			case "earlier":
				s.Turns[0].Reply = ""
				s.Turns[0].Earlier = []string{quote}
			case "unplaced":
				s.Turns[0].Reply = "Unrelated invented answer."
			case "ambiguous":
				s.Turns = append(s.Turns, s.Turns[0])
			case "unknown-start":
				s.Turns[0].Started = time.Time{}
			case "before-start":
				a.RaisedAt = 99
			case "short":
				a.FirstQuote = "Which"
				a.LatestQuote = "Which"
			case "user-text-only":
				s.Turns[0].Reply = ""
				s.Turns[0].Prompt = quote
				s.Turns[0].Items = []transcript.TurnItem{{Kind: transcript.ItemSteer, Text: quote}}
			case "two-quotes-two-turns":
				a.LatestQuote = "Another invented question?"
				s.Turns = append(s.Turns, transcript.Turn{Started: at.Add(time.Millisecond), Reply: a.LatestQuote})
			}
			anchor, err := navPlaceAsk(context.Background(), a, s)
			if err != nil || (anchor != nil) != row.want {
				t.Fatal("placement differs", anchor, err)
			}
			if anchor != nil && *anchor != 100123 {
				t.Fatal("not the exact prompt-start millisecond")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := navPlaceAsk(ctx, state.AskPlacement{}, &transcript.Session{Turns: []transcript.Turn{{Started: at}}}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}

func TestNavAskTurnLiteralIDAdmission(t *testing.T) {
	for _, row := range []struct {
		path string
		ok   bool
	}{
		{"/api/nav/asks/1/turn", true}, {"/api/nav/asks/9223372036854775807/turn", true}, {"/api/nav/asks/0/turn", false}, {"/api/nav/asks/01/turn", false},
		{"/api/nav/asks/9223372036854775808/turn", false}, {"/api/nav/asks/+1/turn", false}, {"/api/nav/asks/1/turn?", false}, {"/api/nav/asks/1/turn?x=1", false}, {"/api/nav/asks/1/turn/", false},
	} {
		req := httptest.NewRequest("GET", row.path, nil)
		_, ok := navAskTurnID(req)
		if ok != row.ok {
			t.Fatal("ID admission differs", row.path)
		}
	}
}
