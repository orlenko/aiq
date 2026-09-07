// Package quota reads aiquota's config and snapshot so `aiq account import`
// can adopt accounts aiquota already knows. Routing does not depend on it.
package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Limit is one normalized aiquota limit.
type Limit struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	Percent       *float64 `json:"percent"`
	ResetsAt      string   `json:"resets_at"`
	Severity      string   `json:"severity"`
	Detail        string   `json:"detail"`
	WindowSeconds int64    `json:"window_seconds"`
}

type Stale struct {
	Since  string `json:"since"`
	Reason string `json:"reason"`
}

// Account is one aiquota snapshot entry.
type Account struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Provider  string   `json:"provider"`
	OK        bool     `json:"ok"`
	Error     string   `json:"error"`
	FetchedAt string   `json:"fetched_at"`
	Limits    []Limit  `json:"limits"`
	Plan      string   `json:"plan"`
	Identity  string   `json:"identity"`
	Notes     []string `json:"notes"`
	Stale     *Stale   `json:"stale"`
}

type Snapshot struct {
	GeneratedAt string    `json:"generated_at"`
	Accounts    []Account `json:"accounts"`
}

// ConfigAccount is one entry of aiquota's config.json.
type ConfigAccount struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Label       string `json:"label"`
	Credentials string `json:"credentials"`
}

type Config struct {
	Accounts []ConfigAccount `json:"accounts"`
	raw      map[string]json.RawMessage
	rawAccts []map[string]json.RawMessage
}

func stateRoot() string {
	if root := os.Getenv("QUOTA_HOME"); root != "" {
		return filepath.Join(root, "state")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "quota-monitor")
}

func configRoot() string {
	if root := os.Getenv("QUOTA_HOME"); root != "" {
		return filepath.Join(root, "config")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "quota-monitor")
}

// SnapshotPath returns the aiquota snapshot location (override wins).
func SnapshotPath(override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(stateRoot(), "snapshot.json")
}

// ConfigPath returns the aiquota config.json location (override wins).
func ConfigPath(override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(configRoot(), "config.json")
}

func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &snap, nil
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &c.raw); err != nil {
		return nil, err
	}
	if accts, ok := c.raw["accounts"]; ok {
		if err := json.Unmarshal(accts, &c.rawAccts); err != nil {
			return nil, fmt.Errorf("parse %s accounts: %w", path, err)
		}
	}
	return &c, nil
}

// SetCredentials repoints one aiquota account at a new credential file and
// writes the config back, preserving unknown keys.
func SetCredentials(path, id, credentials string) error {
	c, err := LoadConfig(path)
	if err != nil {
		return err
	}
	// Edit the raw account objects so keys aiq does not know about survive.
	found := false
	for _, acct := range c.rawAccts {
		var got string
		if json.Unmarshal(acct["id"], &got) == nil && got == id {
			enc, _ := json.Marshal(credentials)
			acct["credentials"] = enc
			found = true
		}
	}
	if !found {
		return fmt.Errorf("aiquota account %q not found in %s", id, path)
	}
	raw := c.raw
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	accounts, err := json.Marshal(c.rawAccts)
	if err != nil {
		return err
	}
	raw["accounts"] = accounts
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(out, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
