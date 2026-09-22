package agy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/state"
)

// Status is the telemetry in the CLI's status-line JSON: the quota buckets
// the account draws on, and who the account is. The CLI pipes it to the
// statusLine command on every state change, including the initializing
// state of a headless run, before the model is called.
type Status struct {
	ConversationID string
	Email          string
	Plan           string
	Model          string
	State          string
	Quota          map[string]Bucket
}

// Bucket is one quota window as the CLI reports it.
type Bucket struct {
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time"`
	ResetInSeconds    int64   `json:"reset_in_seconds"`
}

// ParseStatus reads the status-line payload. ok is false when it carries no
// quota yet (the CLI sends a few payloads while it authenticates).
func ParseStatus(input []byte) (Status, bool) {
	var doc struct {
		ConversationID string            `json:"conversation_id"`
		Email          string            `json:"email"`
		Plan           string            `json:"plan_tier"`
		State          string            `json:"agent_state"`
		Quota          map[string]Bucket `json:"quota"`
		Model          struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	if err := json.Unmarshal(input, &doc); err != nil {
		return Status{}, false
	}
	s := Status{ConversationID: doc.ConversationID, Email: doc.Email, Plan: doc.Plan, Model: doc.Model.ID, State: doc.State, Quota: doc.Quota}
	return s, len(doc.Quota) > 0
}

// Windows converts the quota buckets into aiq windows. Gemini buckets are
// the account's main windows; every other family ("3p" for Claude and
// GPT-OSS models) is a scoped window that binds only a launch on such a
// model.
func (s Status) Windows(now time.Time) []state.Window {
	keys := make([]string, 0, len(s.Quota))
	for k := range s.Quota {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []state.Window
	obs := now.Unix()
	for _, k := range keys {
		b := s.Quota[k]
		family, period, _ := strings.Cut(k, "-")
		kind, secs := state.KindOther, int64(0)
		switch period {
		case "5h":
			kind, secs = state.KindShort, 5*3600
		case "weekly":
			kind, secs = state.KindWeekly, 7*86400
		}
		scope := ""
		label := strings.ToUpper(period[:min(1, len(period))]) + period[min(1, len(period)):]
		if period == "5h" {
			label = "5h"
		}
		if family != "gemini" {
			scope = family
			label = family + " " + strings.ToLower(label)
		}
		used := (1 - b.RemainingFraction) * 100
		if used < 0 {
			used = 0
		}
		var reset int64
		if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
			reset = t.Unix()
		} else if b.ResetInSeconds > 0 {
			reset = obs + b.ResetInSeconds
		}
		severity := ""
		if used >= 100 {
			severity = "critical"
		}
		out = append(out, state.Window{
			Key: family + ":" + period, Label: label, Kind: kind, Scope: scope,
			UsedPct: used, ResetsAt: reset, WindowSeconds: secs, Severity: severity, ObservedAt: obs,
		})
	}
	return out
}

// DefaultStatusLine renders aiq's own line, shown under the CLI's built-in
// one when the user had no statusline command of their own.
func DefaultStatusLine(account string, s Status, ok bool) string {
	if account == "" {
		account = "agy"
	} else {
		account = "agy/" + account
	}
	if !ok {
		return "aiq " + account
	}
	parts := []string{"aiq " + account}
	for _, w := range s.Windows(time.Now()) {
		if w.Scope != "" || w.Kind == state.KindOther {
			continue
		}
		label := "wk"
		if w.Kind == state.KindShort {
			label = "5h"
		}
		part := fmt.Sprintf("%s %.0f%%", label, w.UsedPct)
		if w.Kind == state.KindShort && w.ResetsAt > 0 {
			part += " ↺" + time.Unix(w.ResetsAt, 0).Local().Format("15:04")
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

// --- statusline installation ---

const statuslineMarker = "agy-statusline"

func readSettings(path string) (map[string]any, error) {
	settings := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&settings); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return settings, nil
}

func writeSettings(path string, settings map[string]any) error {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(path, append(out, '\n')); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// InstalledStatusline returns the current statusLine command ("" if none)
// and whether it is already the aiq multiplexer.
func InstalledStatusline(geminiDir string) (command string, installed bool, err error) {
	settings, err := readSettings(SettingsPath(geminiDir))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sl, _ := settings["statusLine"].(map[string]any)
	cmd, _ := sl["command"].(string)
	return cmd, strings.Contains(cmd, statuslineMarker), nil
}

// InstallStatusline points the CLI's statusLine at `<aiqPath>
// agy-statusline`, keeping every other setting. With no statusline command
// of the user's own, aiq's line is stacked under the built-in one rather
// than replacing it. It returns the previous command so the caller can
// persist it for the multiplexer to keep invoking.
func InstallStatusline(geminiDir, aiqPath string) (previous string, err error) {
	path := SettingsPath(geminiDir)
	settings, err := readSettings(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	sl, ok := settings["statusLine"].(map[string]any)
	if !ok {
		sl = map[string]any{}
	}
	if cmd, ok := sl["command"].(string); ok && !strings.Contains(cmd, statuslineMarker) {
		previous = cmd
	}
	if previous == "" {
		if _, set := sl["stack_with_default"]; !set {
			sl["stack_with_default"] = true
		}
	}
	sl["type"] = "command"
	sl["command"] = aiqPath + " agy-statusline"
	settings["statusLine"] = sl
	if err := os.MkdirAll(AppDataDir(geminiDir), 0o755); err != nil {
		return "", err
	}
	return previous, writeSettings(path, settings)
}

// UninstallStatusline restores the previous statusline command (or removes
// the statusLine block when there was none).
func UninstallStatusline(geminiDir, previous string) error {
	path := SettingsPath(geminiDir)
	settings, err := readSettings(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sl, _ := settings["statusLine"].(map[string]any)
	if sl == nil {
		return nil
	}
	if previous != "" {
		sl["command"] = previous
	} else {
		delete(settings, "statusLine")
	}
	return writeSettings(path, settings)
}
