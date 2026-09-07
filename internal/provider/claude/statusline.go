package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/fsutil"
)

// readSettings parses settings.json keeping numbers verbatim.
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

// writeSettings writes settings.json atomically, world-readable like Claude
// Code leaves it.
func writeSettings(path string, settings map[string]any) error {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(path, append(out, '\n')); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}

// RateLimits is the telemetry extracted from Claude Code's status-line JSON.
type RateLimits struct {
	FiveHourPct   float64 // -1 unknown
	FiveHourReset int64   // unix seconds, 0 unknown
	SevenDayPct   float64
	SevenDayReset int64
}

// ParseRateLimits extracts rate_limits from status-line stdin JSON. ok is
// false when the payload has no usable rate-limit data.
func ParseRateLimits(input []byte) (RateLimits, bool) {
	rl := RateLimits{FiveHourPct: -1, SevenDayPct: -1}
	var doc struct {
		RateLimits map[string]json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(input, &doc); err != nil || doc.RateLimits == nil {
		return rl, false
	}
	ok := false
	if pct, reset, found := parseWindow(doc.RateLimits["five_hour"]); found {
		rl.FiveHourPct, rl.FiveHourReset = pct, reset
		ok = true
	}
	if pct, reset, found := parseWindow(doc.RateLimits["seven_day"]); found {
		rl.SevenDayPct, rl.SevenDayReset = pct, reset
		ok = true
	}
	return rl, ok
}

func parseWindow(raw json.RawMessage) (pct float64, reset int64, ok bool) {
	pct = -1
	if len(raw) == 0 {
		return pct, 0, false
	}
	var w struct {
		UsedPercentage *float64        `json:"used_percentage"`
		ResetsAt       json.RawMessage `json:"resets_at"`
	}
	if err := json.Unmarshal(raw, &w); err != nil || w.UsedPercentage == nil {
		return pct, 0, false
	}
	return *w.UsedPercentage, parseTimestamp(w.ResetsAt), true
}

// parseTimestamp accepts unix seconds (number or numeric string) or RFC3339.
func parseTimestamp(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return int64(n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0
	}
	if u, err := strconv.ParseInt(s, 10, 64); err == nil {
		return u
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}

// DefaultStatusLine renders aiq's own status line, used when the user had no
// statusline of their own.
func DefaultStatusLine(account string, rl RateLimits, ok bool) string {
	if account == "" {
		account = "claude"
	} else {
		account = "claude/" + account
	}
	if !ok {
		return account
	}
	parts := []string{account}
	if rl.FiveHourPct >= 0 {
		s := fmt.Sprintf("5h %.0f%%", rl.FiveHourPct)
		if rl.FiveHourReset > 0 {
			s += " ↺" + time.Unix(rl.FiveHourReset, 0).Local().Format("15:04")
		}
		parts = append(parts, s)
	}
	if rl.SevenDayPct >= 0 {
		parts = append(parts, fmt.Sprintf("wk %.0f%%", rl.SevenDayPct))
	}
	return strings.Join(parts, " · ")
}

// --- statusline installation ---

const statuslineMarker = "claude-statusline"

// SettingsPath returns Claude Code's settings.json path for a config dir.
func SettingsPath(configDir string) string {
	return filepath.Join(configDir, "settings.json")
}

// InstalledStatusline returns the current statusLine command ("" if none)
// and whether it is already the aiq multiplexer.
func InstalledStatusline(configDir string) (command string, installed bool, err error) {
	settings, err := readSettings(SettingsPath(configDir))
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

// InstallStatusline points Claude Code's statusLine at `<aiqPath>
// claude-statusline`, preserving all other settings. It returns the previous
// command so the caller can persist it for the multiplexer to keep invoking.
func InstallStatusline(configDir, aiqPath string) (previous string, err error) {
	path := SettingsPath(configDir)
	settings, err := readSettings(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	// Merge into any existing statusLine object so optional keys like
	// padding/refreshInterval/hideVimModeIndicator survive.
	sl, ok := settings["statusLine"].(map[string]any)
	if !ok {
		sl = map[string]any{}
	}
	if cmd, ok := sl["command"].(string); ok && !strings.Contains(cmd, statuslineMarker) {
		previous = cmd
	}
	sl["type"] = "command"
	sl["command"] = aiqPath + " claude-statusline"
	settings["statusLine"] = sl
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", err
	}
	return previous, writeSettings(path, settings)
}

// UninstallStatusline restores the previous statusline command (or removes
// the statusLine block when there was none).
func UninstallStatusline(configDir, previous string) error {
	path := SettingsPath(configDir)
	settings, err := readSettings(path)
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
