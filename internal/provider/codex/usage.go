package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// Usage is what one poll of a Codex account yields.
type Usage struct {
	Windows      []state.Window
	ResetCredits int
	Identity     string
	Plan         string
}

// Poll reads rate limits through `codex app-server` (the documented API),
// letting Codex refresh its own credential, and reads the identity from the
// stored id token.
func (p *Provider) Poll(home string, native bool, now time.Time) (*Usage, error) {
	rl, err := p.Probe(home, native)
	if err != nil {
		return nil, err
	}
	u := &Usage{ResetCredits: rl.ResetCredits}
	u.Windows = WindowsFromRateLimits(rl, now)
	u.Identity, u.Plan = Identity(AuthPath(home))
	return u, nil
}

// WindowsFromRateLimits maps the app-server result onto windows with the
// keys aiquota uses for Codex: "plan:primary_window", "plan:secondary_window".
func WindowsFromRateLimits(rl RateLimits, now time.Time) []state.Window {
	var out []state.Window
	obs := now.Unix()
	add := func(key string, pct float64, reset, secs int64) {
		if pct < 0 {
			return
		}
		kind := state.KindShort
		switch {
		case secs >= 3*86400:
			kind = state.KindWeekly
		case secs == 0 && reset > 0 && time.Unix(reset, 0).Sub(now) > 30*time.Hour:
			kind = state.KindWeekly
		}
		label := "5h"
		if kind == state.KindWeekly {
			label = "Weekly"
		} else if secs > 0 && secs != 5*3600 {
			label = fmt.Sprintf("%dh", secs/3600)
			if secs < 3600 {
				label = fmt.Sprintf("%dm", secs/60)
			}
		}
		severity := ""
		if pct >= 100 {
			severity = "critical"
		}
		out = append(out, state.Window{
			Key: key, Label: label, Kind: kind, UsedPct: pct, ResetsAt: reset,
			WindowSeconds: secs, Severity: severity, ObservedAt: obs,
		})
	}
	add("plan:primary_window", rl.PrimaryUsedPct, rl.PrimaryReset, rl.PrimaryWindowSecs)
	add("plan:secondary_window", rl.SecondaryUsedPct, rl.SecondaryReset, rl.SecondaryWindowSec)
	return out
}

// Identity decodes the email and plan from the id token in auth.json.
func Identity(authPath string) (email, plan string) {
	data, err := os.ReadFile(authPath)
	if err != nil {
		return "", ""
	}
	var doc struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.Tokens.IDToken == "" {
		return "", ""
	}
	parts := strings.Split(doc.Tokens.IDToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	if e, ok := claims["email"].(string); ok {
		email = e
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if p, ok := auth["chatgpt_plan_type"].(string); ok {
			plan = p
		}
		if email == "" {
			if e, ok := auth["email"].(string); ok {
				email = e
			}
		}
	}
	if email == "" {
		if profile, ok := claims["https://api.openai.com/profile"].(map[string]any); ok {
			if e, ok := profile["email"].(string); ok {
				email = e
			}
		}
	}
	return email, plan
}
