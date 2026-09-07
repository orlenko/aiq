package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
)

func cmdMark(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: aiq mark <provider>/<name> exhausted [--until <time>] | ready")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	acc, err := a.st.GetAccount(args[0])
	if err != nil {
		return err
	}
	now := time.Now()
	switch args[1] {
	case "exhausted":
		until := time.Time{}
		for i := 2; i < len(args); i++ {
			if args[i] == "--until" && i+1 < len(args) {
				until, err = parseUntil(args[i+1], now)
				if err != nil {
					return err
				}
			}
		}
		if until.IsZero() {
			until = a.pool.ExhaustUntil(acc, "marked by user", 5*time.Hour, now)
		} else {
			a.st.MarkExhausted(acc.ID, "marked by user", until, now)
			a.st.LogEvent(acc.Provider, acc.ID, "exhausted", "marked by user until "+until.Local().Format("Mon 15:04"), now)
		}
		daemon.Notify(a.cfg.Daemon.Listen, acc.ID)
		fmt.Printf("%s exhausted until %s\n", acc.ID, until.Local().Format("Mon 15:04"))
	case "ready":
		if err := a.st.MarkReady(acc.ID, now); err != nil {
			return err
		}
		a.st.LogEvent(acc.Provider, acc.ID, "ready", "marked by user", now)
		fmt.Printf("%s ready\n", acc.ID)
	default:
		return fmt.Errorf("state must be exhausted or ready")
	}
	return nil
}

// parseUntil accepts "+2h", "14:42" (next occurrence, local) or RFC3339.
func parseUntil(s string, now time.Time) (time.Time, error) {
	if strings.HasPrefix(s, "+") {
		d, err := time.ParseDuration(s[1:])
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("15:04", s, time.Local); err == nil {
		y, m, d := now.Date()
		t = time.Date(y, m, d, t.Hour(), t.Minute(), 0, 0, time.Local)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (want +2h, 14:42, or RFC3339)", s)
}
