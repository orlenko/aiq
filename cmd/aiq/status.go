package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

func cmdStatus(args []string) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	if hasFlag(args, "--refresh") {
		for _, r := range a.pool.Refresh() {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "aiq: %s: %v\n", r.ID, r.Err)
			}
		}
	}
	view, err := a.pool.View(15)
	if err != nil {
		return err
	}
	if hasFlag(args, "--json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	fmt.Print(renderStatus(view, hasFlag(args, "--explain")))
	return nil
}

func cmdTop(args []string) error {
	interval := 5 * time.Second
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	for {
		a, err := openApp()
		if err != nil {
			return err
		}
		view, err := a.pool.View(8)
		a.close()
		if err != nil {
			return err
		}
		fmt.Print("\033[H\033[2J")
		fmt.Print(renderStatus(view, hasFlag(args, "--explain")))
		alive := "down"
		if daemon.Alive(a.cfg.Daemon.Listen) {
			alive = "http://" + a.cfg.Daemon.Listen + "/"
		}
		fmt.Printf("\n  daemon %s · %s · ctrl-c to quit\n", alive, time.Now().Format("15:04:05"))
		select {
		case <-stop:
			return nil
		case <-time.After(interval):
		}
	}
}

func bar(pct float64, width int) string {
	if pct < 0 {
		return strings.Repeat("·", width)
	}
	filled := int(pct/100*float64(width) + 0.5)
	if pct > 0 && filled == 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("─", width-filled)
}

func fmtIn(ts int64, now time.Time) string {
	if ts == 0 {
		return ""
	}
	d := time.Unix(ts, 0).Sub(now)
	switch {
	case d <= 0:
		return "due"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return time.Unix(ts, 0).Local().Format("Mon 15:04")
	}
}

func fmtAgo(ts int64, now time.Time) string {
	if ts == 0 {
		return "never"
	}
	d := now.Sub(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func renderStatus(v *pool.View, explain bool) string {
	now := time.Now()
	var b strings.Builder
	if len(v.Accounts) == 0 {
		b.WriteString("no accounts — run: aiq account import\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-20s %-9s %-28s %5s  %s\n", "ACCOUNT", "STATE", "WINDOWS", "SCORE", "LEASES / NOTES")
	for _, acc := range v.Accounts {
		st := "ready"
		switch {
		case !acc.Enabled:
			st = "disabled"
		case !acc.HasCredential:
			st = "NO LOGIN"
		case acc.Exhausted:
			st = "EXHAUSTED"
		case !acc.Eligible:
			st = "held"
		}
		score := "  -"
		if acc.Eligible {
			score = fmt.Sprintf("%5.1f", acc.Score)
		}
		var notes []string
		if acc.Interactive > 0 {
			notes = append(notes, fmt.Sprintf("%d interactive", acc.Interactive))
		}
		if acc.Workers > 0 {
			notes = append(notes, fmt.Sprintf("%d worker", acc.Workers))
		}
		if acc.Exhausted && acc.Reason != "" {
			notes = append(notes, acc.Reason)
		} else if !acc.Eligible && acc.Ineligible != "" {
			notes = append(notes, acc.Ineligible)
		}
		if acc.ResetCredits > 0 {
			notes = append(notes, fmt.Sprintf("%d reset credit", acc.ResetCredits))
		}
		if acc.PollError != "" {
			notes = append(notes, acc.PollError)
		}
		first := true
		wrote := false
		for _, w := range acc.Windows {
			if !w.Binding {
				continue
			}
			label := w.Label
			if label == "" {
				label = w.Key
			}
			if len(label) > 8 {
				label = label[:8]
			}
			pct := "  -"
			if w.UsedPct >= 0 {
				pct = fmt.Sprintf("%3.0f", w.UsedPct)
			}
			win := fmt.Sprintf("%-8s %s %s%% %s", label, bar(w.UsedPct, 8), pct, fmtIn(w.ResetsAt, now))
			if first {
				fmt.Fprintf(&b, "%-20s %-9s %-28s %5s  %s\n", acc.ID, st, win, score, strings.Join(notes, " · "))
				first = false
			} else {
				fmt.Fprintf(&b, "%-20s %-9s %-28s\n", "", "", win)
			}
			wrote = true
		}
		if !wrote {
			fmt.Fprintf(&b, "%-20s %-9s %-28s %5s  %s\n", acc.ID, st, "no telemetry", score, strings.Join(notes, " · "))
		}
		if acc.Identity != "" || acc.ObservedAt > 0 {
			fmt.Fprintf(&b, "%-20s %-9s %s · seen %s\n", "", "", acc.Identity, fmtAgo(acc.ObservedAt, now))
		}
	}
	if explain {
		b.WriteString("\nnext pick (score = Σ remaining%/hours-to-reset, higher first):\n")
		keys := []string{"claude/" + state.ModeInteractive, "claude/" + state.ModeWorker, "codex/" + state.ModeInteractive, "codex/" + state.ModeWorker}
		for _, k := range keys {
			list, ok := v.Rankings[k]
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "  %s\n", k)
			for i, r := range list {
				if r.Eligible {
					fmt.Fprintf(&b, "    %d. %-20s %6.1f/h  %s\n", i+1, r.ID, r.Score, strings.Join(r.Terms, " + "))
				} else {
					fmt.Fprintf(&b, "    %d. %-20s      -    %s\n", i+1, r.ID, r.Reason)
				}
			}
		}
	}
	if len(v.Events) > 0 {
		b.WriteString("\nrecent:\n")
		for _, e := range v.Events {
			fmt.Fprintf(&b, "  %s  %-20s %-10s %s\n", time.Unix(e.Timestamp, 0).Local().Format("Jan 2 15:04"), e.AccountID, e.Type, e.Detail)
		}
	}
	return b.String()
}

// selectorRank is a thin alias so account.go does not import selector twice.
func selectorRank(p selector.Policy, c []selector.Candidate) []selector.Ranked {
	return selector.Rank(p, c)
}
