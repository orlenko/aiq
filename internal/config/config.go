// Package config loads and saves aiq's TOML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/orlenko/aiq/internal/paths"
)

type Provider struct {
	// Binary is the real CLI path. Empty means: walk PATH, skipping aiq's
	// own shim directory.
	Binary string `toml:"binary,omitempty"`
	// ModelScope names a model-scoped weekly limit that counts as binding
	// (Claude reports e.g. a "Fable" weekly cap next to the overall one).
	// "auto" reads the default model from the provider's own settings; ""
	// ignores scoped limits.
	ModelScope string `toml:"model_scope"`
}

type Selection struct {
	// InteractivePolicy is "sticky" (keep the workspace's account until it
	// crosses SwitchPct) or "score" (always take the best-scoring account).
	InteractivePolicy string `toml:"interactive_policy"`
	// SwitchPct is the used-percentage at which a sticky account is left.
	SwitchPct float64 `toml:"switch_pct"`
	// StaleAfterSeconds is how old telemetry may be before scoring falls back
	// to a neutral prior.
	StaleAfterSeconds int64 `toml:"stale_after_seconds"`
	// WeeklyReservePct: workers skip accounts whose weekly remaining is at or
	// below this, keeping a floor for interactive use.
	WeeklyReservePct float64 `toml:"weekly_reserve_pct"`
	// MaxWorkersPerAccount caps concurrent worker leases on one account.
	MaxWorkersPerAccount int `toml:"max_workers_per_account"`
	// MaxDepth caps nested AI invocations (claude → codex → claude ...).
	MaxDepth int `toml:"max_depth"`
	// MinHours floors the time-to-reset used in perishability scoring.
	MinHours float64 `toml:"min_hours"`
	// WeeklyWeight multiplies the weekly window's term: one weekly point is
	// worth several session points of tokens, so an approaching weekly reset
	// starts to matter hours before it happens, not minutes.
	WeeklyWeight float64 `toml:"weekly_weight"`
}

type Worker struct {
	// Retry reroutes a worker to the next account when the provider rejects
	// it with a usage-limit message before doing any work.
	Retry bool `toml:"retry"`
	// RetryMaxSeconds: a run that lasted longer than this is never retried,
	// since it may have already changed files.
	RetryMaxSeconds float64 `toml:"retry_max_seconds"`
	// LimitPatterns are regexps matched against a worker's combined output.
	LimitPatterns []string `toml:"limit_patterns"`
	// WaitForSlotSeconds: a worker that finds every eligible account at its
	// worker cap polls for a free slot this long before refusing (0 = refuse
	// at once). `aiq run --wait` overrides it per launch.
	WaitForSlotSeconds int64 `toml:"wait_for_slot_seconds"`
}

type Poll struct {
	// IntervalSeconds is the daemon's polling period.
	IntervalSeconds int64 `toml:"interval_seconds"`
	// TimeoutSeconds bounds one account's poll.
	TimeoutSeconds int64 `toml:"timeout_seconds"`
}

// Aiquota points at an aiquota installation, used only by
// `aiq account import` to adopt accounts. Not needed for routing.
type Aiquota struct {
	Config   string `toml:"config,omitempty"`
	Snapshot string `toml:"snapshot,omitempty"`
}

// Display controls the status page and `aiq status`: accounts render one
// column per provider, each column in Order; Labels replace the email under
// the account id. Edit by hand or with `aiq account order` / `aiq account label`.
type Display struct {
	Order  []string          `toml:"order"`
	Labels map[string]string `toml:"labels"`
}

type Daemon struct {
	Listen string `toml:"listen"`
}

// Long configures supervised long-running sessions (`aiq long`).
type Long struct {
	// DrainPct: when the account's tightest binding window has this much or
	// less remaining, the session is asked to wrap up.
	DrainPct float64 `toml:"drain_pct"`
	// Fallback is the provider order for a takeover when the session's own
	// provider has no eligible account left.
	Fallback []string `toml:"fallback"`
	// CheckIntervalSeconds is how often the daemon evaluates long sessions.
	CheckIntervalSeconds int64 `toml:"check_interval_seconds"`
	// IdleGraceSeconds: an idle session (no turn in progress) is respawned
	// this long after a drain request without waiting for a wrap-up turn.
	IdleGraceSeconds int64 `toml:"idle_grace_seconds"`
	// TmuxPrefix names the tmux sessions: <prefix>-<workspace basename>.
	TmuxPrefix string `toml:"tmux_prefix"`
}

type Telemetry struct {
	ClaudeStatusline bool `toml:"claude_statusline"`
	// ClaudeStatuslineInstalled records that the multiplexer was installed
	// once; removing it from settings.json afterwards is respected.
	ClaudeStatuslineInstalled bool `toml:"claude_statusline_installed,omitempty"`
	// ClaudeStatuslinePrevious preserves the user's own statusline command so
	// the multiplexer can keep invoking it.
	ClaudeStatuslinePrevious string `toml:"claude_statusline_previous,omitempty"`
}

type Config struct {
	Version   int `toml:"version"`
	Providers struct {
		Claude Provider `toml:"claude"`
		Codex  Provider `toml:"codex"`
	} `toml:"providers"`
	Selection Selection `toml:"selection"`
	Worker    Worker    `toml:"worker"`
	Poll      Poll      `toml:"poll"`
	Aiquota   Aiquota   `toml:"aiquota"`
	Display   Display   `toml:"display"`
	Daemon    Daemon    `toml:"daemon"`
	Long      Long      `toml:"long"`
	Telemetry Telemetry `toml:"telemetry"`
}

// DefaultLimitPatterns match the usage-limit rejections Claude Code and Codex
// print before doing any work.
var DefaultLimitPatterns = []string{
	`(?i)hit your (usage|session|weekly|5-hour|5 hour) limit`,
	`(?i)usage limit (reached|exceeded)`,
	`(?i)you('ve| have) (reached|exceeded) (your|the) (usage|rate|weekly|session) limit`,
	`(?i)out of (codex|claude) messages`,
	`(?i)rate[_ ]limit[_ ]exceeded`,
	`(?i)"type":\s*"rate_limit_error"`,
}

func Default() *Config {
	c := &Config{Version: 2}
	c.Providers.Claude.ModelScope = "auto"
	c.Providers.Codex.ModelScope = "auto"
	c.Selection = Selection{
		InteractivePolicy:    "sticky",
		SwitchPct:            95,
		StaleAfterSeconds:    900,
		WeeklyReservePct:     5,
		MaxWorkersPerAccount: 3,
		MaxDepth:             3,
		MinHours:             0.25,
		WeeklyWeight:         5,
	}
	c.Worker = Worker{Retry: true, RetryMaxSeconds: 60}
	c.Poll = Poll{IntervalSeconds: 300, TimeoutSeconds: 60}
	c.Display.Labels = map[string]string{}
	c.Daemon.Listen = "127.0.0.1:7379"
	c.Long = Long{DrainPct: 4, Fallback: []string{"claude", "codex"}, CheckIntervalSeconds: 30, IdleGraceSeconds: 20, TmuxPrefix: "aiq"}
	c.Telemetry.ClaudeStatusline = true
	return c
}

// Load reads the config file, applying defaults for anything unset.
// A missing file returns defaults without error.
func Load() (*Config, error) {
	c := Default()
	data, err := os.ReadFile(paths.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := toml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", paths.ConfigFile(), err)
	}
	if c.Selection.MinHours <= 0 {
		c.Selection.MinHours = 0.25
	}
	if c.Selection.WeeklyWeight <= 0 {
		c.Selection.WeeklyWeight = 5
	}
	if c.Poll.IntervalSeconds <= 0 {
		c.Poll.IntervalSeconds = 300
	}
	if c.Poll.TimeoutSeconds <= 0 {
		c.Poll.TimeoutSeconds = 60
	}
	if c.Display.Labels == nil {
		c.Display.Labels = map[string]string{}
	}
	if c.Long.DrainPct <= 0 {
		c.Long.DrainPct = 4
	}
	if len(c.Long.Fallback) == 0 {
		c.Long.Fallback = []string{"claude", "codex"}
	}
	if c.Long.CheckIntervalSeconds <= 0 {
		c.Long.CheckIntervalSeconds = 30
	}
	if c.Long.IdleGraceSeconds <= 0 {
		c.Long.IdleGraceSeconds = 20
	}
	if c.Long.TmuxPrefix == "" {
		c.Long.TmuxPrefix = "aiq"
	}
	return c, nil
}

// LimitPatterns returns the configured patterns, or the defaults.
func (c *Config) LimitPatterns() []string {
	if len(c.Worker.LimitPatterns) > 0 {
		return c.Worker.LimitPatterns
	}
	return DefaultLimitPatterns
}

// Save writes the config atomically.
func Save(c *Config) error {
	path := paths.ConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(c); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
