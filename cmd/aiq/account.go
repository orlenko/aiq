package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/quota"
	"github.com/orlenko/aiq/internal/state"
)

func cmdAccount(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq account list|add|login|authorize|poll|label|order|enable|disable|remove|use|next|import|link")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return a.accountList()
	case "add":
		if len(rest) < 2 {
			return fmt.Errorf("usage: aiq account add <claude|codex> <name> [--no-login]")
		}
		return a.accountAdd(rest[0], rest[1], !hasFlag(rest, "--no-login"))
	case "login":
		if len(rest) < 1 {
			return fmt.Errorf("usage: aiq account login <provider>/<name>")
		}
		return a.accountLogin(rest[0])
	case "authorize":
		if len(rest) < 1 {
			return fmt.Errorf("usage: aiq account authorize claude/<name>")
		}
		return a.accountAuthorize(rest[0])
	case "poll":
		return a.pollAndReport(rest...)
	case "label":
		if len(rest) < 2 {
			return fmt.Errorf("usage: aiq account label <provider>/<name> <text>")
		}
		return a.accountLabel(rest[0], strings.Join(rest[1:], " "))
	case "order":
		return a.accountOrder(rest)
	case "enable", "disable":
		if len(rest) < 1 {
			return fmt.Errorf("usage: aiq account %s <provider>/<name>", sub)
		}
		if err := a.st.SetEnabled(rest[0], sub == "enable"); err != nil {
			return err
		}
		fmt.Printf("%s %sd\n", rest[0], sub)
		return nil
	case "remove":
		if len(rest) < 1 {
			return fmt.Errorf("usage: aiq account remove <provider>/<name> [--purge]")
		}
		return a.accountRemove(rest[0], hasFlag(rest, "--purge"))
	case "use":
		if len(rest) < 1 {
			return fmt.Errorf("usage: aiq account use <provider>/<name>")
		}
		acc, err := a.st.GetAccount(rest[0])
		if err != nil {
			return err
		}
		ws := workspaceID()
		if err := a.st.SetAffinity(acc.Provider, ws, acc.ID, time.Now()); err != nil {
			return err
		}
		fmt.Printf("%s pinned to %s\n", ws, acc.ID)
		return nil
	case "next":
		if len(rest) < 1 {
			return fmt.Errorf("usage: aiq account next <provider>")
		}
		return a.accountNext(rest[0])
	case "import":
		return a.accountImport(rest)
	case "link":
		if len(rest) < 2 {
			return fmt.Errorf("usage: aiq account link <provider>/<name> <aiquota-id>")
		}
		return a.accountLink(rest[0], rest[1])
	}
	return fmt.Errorf("unknown account subcommand %q", sub)
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func (a *app) accountList() error {
	accounts, err := a.st.ListAccounts("")
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("no accounts — run: aiq account add <claude|codex> <name>")
		return nil
	}
	fmt.Printf("%-20s %-8s %-6s %-6s %-30s %-24s %s\n", "ACCOUNT", "STATE", "LOGIN", "POLL", "IDENTITY", "LABEL", "HOME")
	for _, acc := range accounts {
		st := "enabled"
		if !acc.Enabled {
			st = "disabled"
		}
		login := "yes"
		if !pool.HasCredential(acc) {
			login = "NO"
		}
		poll := "yes"
		if acc.Provider == "claude" && !claude.HasGrant(acc.Home) {
			poll = "NO"
		}
		home := acc.Home
		if acc.Native {
			home += " (native)"
		}
		fmt.Printf("%-20s %-8s %-6s %-6s %-30s %-24s %s\n", acc.ID, st, login, poll, acc.Identity, a.cfg.Display.Labels[acc.ID], shortHome(home))
	}
	return nil
}

func shortHome(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func homeFor(provider, name string) string {
	switch provider {
	case "claude":
		return filepath.Join(paths.ClaudeHomesDir(), name)
	default:
		return filepath.Join(paths.CodexHomesDir(), name)
	}
}

func validName(name string) error {
	if name == "" || strings.ContainsAny(name, "/ \t\n") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid account name %q", name)
	}
	return nil
}

// accountAdd creates an overlay home for a new account, logs the CLI in,
// and (Claude) authorizes the poll grant.
func (a *app) accountAdd(provider, name string, login bool) error {
	if provider != "claude" && provider != "codex" {
		return fmt.Errorf("provider must be claude or codex")
	}
	if err := validName(name); err != nil {
		return err
	}
	id := state.AccountID(provider, name)
	if _, err := a.st.GetAccount(id); err == nil {
		return fmt.Errorf("%s already exists", id)
	}
	acc := state.Account{
		ID: id, Provider: provider, Name: name, Enabled: true,
		Home: homeFor(provider, name), Priority: 100, CreatedAt: time.Now().Unix(),
	}
	if err := a.prepareHome(acc); err != nil {
		return err
	}
	if err := a.st.AddAccount(acc); err != nil {
		return err
	}
	a.appendOrder(id)
	fmt.Printf("added %s (home %s)\n", id, shortHome(acc.Home))
	if !login {
		return nil
	}
	if err := a.accountLogin(id); err != nil {
		return err
	}
	if provider == "claude" {
		if err := a.accountAuthorize(id); err != nil {
			return err
		}
	}
	return a.pollAndReport(id)
}

// accountLogin runs the provider's browser login inside the account home.
func (a *app) accountLogin(id string) error {
	acc, err := a.st.GetAccount(id)
	if err != nil {
		return err
	}
	if err := a.prepareHome(acc); err != nil {
		return err
	}
	switch acc.Provider {
	case "claude":
		p, err := a.claudeProvider()
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Step 1/2 for %s: Claude Code login.\n", id)
		if err := p.Login(acc.Home, acc.Native); err != nil {
			return err
		}
		email, ok, err := p.AuthStatus(acc.Home, acc.Native)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("login did not complete for %s", id)
		}
		if !acc.Native {
			claude.MarkLoggedIn(acc.Home, email)
		}
		acc.Identity = email
	case "codex":
		p, err := a.codexProvider()
		if err != nil {
			return err
		}
		if err := p.Login(acc.Home, acc.Native); err != nil {
			return err
		}
		if email, _ := codex.Identity(codex.AuthPath(acc.Home)); email != "" {
			acc.Identity = email
		}
	}
	a.st.UpdateAccount(acc)
	a.st.LogEvent(acc.Provider, acc.ID, "login", acc.Identity, time.Now())
	a.warnDuplicateIdentity(acc)
	fmt.Printf("%s logged in%s\n", id, identitySuffix(acc.Identity))
	return nil
}

// accountAuthorize mints aiq's own poll grant for a Claude account.
func (a *app) accountAuthorize(id string) error {
	acc, err := a.st.GetAccount(id)
	if err != nil {
		return err
	}
	if acc.Provider != "claude" {
		return fmt.Errorf("only Claude accounts need a poll grant; Codex is polled through its own CLI")
	}
	fmt.Fprintf(os.Stderr, "Step 2/2 for %s: quota polling grant.\n", id)
	g, err := claude.Authorize(os.Stdin, os.Stderr)
	if err != nil {
		return err
	}
	if err := claude.SaveGrant(claude.GrantPath(acc.Home), g); err != nil {
		return err
	}
	u, err := claude.Poll(claude.GrantPath(acc.Home), time.Now())
	if err != nil {
		return fmt.Errorf("grant saved but the first poll failed: %w", err)
	}
	if u.Identity != "" {
		if acc.Identity != "" && !strings.EqualFold(acc.Identity, u.Identity) {
			fmt.Fprintf(os.Stderr, "WARNING: Claude Code in %s is logged in as %s but the poll grant belongs to %s. Redo one of them so they match: aiq account login %s / aiq account authorize %s\n",
				id, acc.Identity, u.Identity, id, id)
		}
		if acc.Identity == "" {
			acc.Identity = u.Identity
			a.st.UpdateAccount(acc)
		}
	}
	a.st.LogEvent("claude", acc.ID, "authorize", u.Identity, time.Now())
	fmt.Printf("%s poll grant saved%s\n", id, identitySuffix(u.Identity))
	return nil
}

func (a *app) pollAndReport(ids ...string) error {
	results := a.pool.Refresh(ids...)
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Printf("%-20s %s\n", r.ID, r.Err)
		} else {
			fmt.Printf("%-20s polled\n", r.ID)
		}
	}
	if failed > 0 && failed == len(results) {
		return fmt.Errorf("every poll failed")
	}
	return nil
}

func identitySuffix(email string) string {
	if email == "" {
		return ""
	}
	return " as " + email
}

// warnDuplicateIdentity flags two accounts that authenticated as one login.
func (a *app) warnDuplicateIdentity(acc state.Account) {
	if acc.Identity == "" {
		return
	}
	accounts, _ := a.st.ListAccounts(acc.Provider)
	for _, other := range accounts {
		if other.ID != acc.ID && strings.EqualFold(other.Identity, acc.Identity) {
			fmt.Fprintf(os.Stderr, "WARNING: %s and %s are both %s — the browser authorized the account it was already signed in as. Sign out of the provider and run: aiq account login %s\n",
				acc.ID, other.ID, acc.Identity, acc.ID)
		}
	}
}

// --- display config ---

func (a *app) appendOrder(id string) {
	for _, o := range a.cfg.Display.Order {
		if o == id {
			return
		}
	}
	a.cfg.Display.Order = append(a.cfg.Display.Order, id)
	config.Save(a.cfg)
}

func (a *app) accountLabel(id, label string) error {
	if _, err := a.st.GetAccount(id); err != nil {
		return err
	}
	if label == "" || label == "-" {
		delete(a.cfg.Display.Labels, id)
	} else {
		a.cfg.Display.Labels[id] = label
	}
	if err := config.Save(a.cfg); err != nil {
		return err
	}
	fmt.Printf("%s label: %q (in %s)\n", id, a.cfg.Display.Labels[id], paths.ConfigFile())
	return nil
}

// accountOrder sets the display order; with no ids it prints the current one.
func (a *app) accountOrder(ids []string) error {
	accounts, err := a.st.ListAccounts("")
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		view, err := a.pool.View(0)
		if err != nil {
			return err
		}
		var all []string
		for _, v := range view.Accounts {
			all = append(all, v.ID)
			fmt.Printf("%-20s %s\n", v.ID, a.cfg.Display.Labels[v.ID])
		}
		fmt.Printf("\nreorder with:\n  aiq account order %s\n(or edit [display] order in %s)\n", strings.Join(all, " "), paths.ConfigFile())
		return nil
	}
	known := map[string]bool{}
	for _, acc := range accounts {
		known[acc.ID] = true
	}
	seen := map[string]bool{}
	var order []string
	for _, id := range ids {
		if !known[id] {
			return fmt.Errorf("no such account: %s", id)
		}
		if seen[id] {
			return fmt.Errorf("repeated: %s", id)
		}
		seen[id] = true
		order = append(order, id)
	}
	// Anything not named keeps its relative position, after the named ones.
	for _, id := range a.cfg.Display.Order {
		if known[id] && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	for _, acc := range accounts {
		if !seen[acc.ID] {
			order = append(order, acc.ID)
		}
	}
	a.cfg.Display.Order = order
	if err := config.Save(a.cfg); err != nil {
		return err
	}
	fmt.Printf("order: %s\n", strings.Join(order, " · "))
	return nil
}

// --- lifecycle ---

func (a *app) accountRemove(id string, purge bool) error {
	acc, err := a.st.GetAccount(id)
	if err != nil {
		return err
	}
	if err := a.st.RemoveAccount(id); err != nil {
		return err
	}
	var order []string
	for _, o := range a.cfg.Display.Order {
		if o != id {
			order = append(order, o)
		}
	}
	a.cfg.Display.Order = order
	delete(a.cfg.Display.Labels, id)
	config.Save(a.cfg)
	fmt.Printf("removed %s\n", id)
	if purge && !acc.Native && strings.HasPrefix(acc.Home, paths.DataDir()) {
		if err := os.RemoveAll(acc.Home); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", shortHome(acc.Home))
	} else if !acc.Native {
		fmt.Printf("home kept at %s (pass --purge to delete it)\n", shortHome(acc.Home))
	}
	return nil
}

func (a *app) accountNext(provider string) error {
	ws := workspaceID()
	current, _ := a.st.GetAffinity(provider, ws)
	cands, err := a.pool.Candidates(provider)
	if err != nil {
		return err
	}
	pol := a.pool.Policy(provider, state.ModeInteractive, time.Now())
	pol.Sticky = false
	pol.SkipID = current
	ranked := selectorRank(pol, cands)
	for _, r := range ranked {
		if r.Eligible {
			a.st.SetAffinity(provider, ws, r.ID, time.Now())
			fmt.Printf("%s now uses %s (was %s)\n", ws, r.ID, orNone(current))
			return nil
		}
	}
	return fmt.Errorf("no other eligible %s account", provider)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// --- import from aiquota (optional convenience on machines that have it) ---

// accountImport adopts aiquota accounts. Codex credentials and Claude poll
// grants are copied into the aiq homes and aiquota's config is re-pointed at
// them, so one file is refreshed by both tools. Labels and grid order come
// along into [display]. The Claude account matching the current ~/.claude
// login is registered as native; the others need `aiq account login`.
func (a *app) accountImport(args []string) error {
	cfgPath := quota.ConfigPath(a.cfg.Aiquota.Config)
	qcfg, err := quota.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("aiquota config: %w (aiquota is optional; use `aiq account add` instead)", err)
	}
	identity := map[string]string{}
	if snap, err := quota.LoadSnapshot(quota.SnapshotPath(a.cfg.Aiquota.Snapshot)); err == nil {
		for _, qa := range snap.Accounts {
			identity[qa.ID] = qa.Identity
		}
	}
	names := map[string]string{}
	var only []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		id, name, _ := strings.Cut(arg, "=")
		only = append(only, id)
		if name != "" {
			names[id] = name
		}
	}
	wanted := func(id string) bool {
		if len(only) == 0 {
			return true
		}
		for _, o := range only {
			if o == id {
				return true
			}
		}
		return false
	}

	nativeClaude := ""
	if p, err := a.claudeProvider(); err == nil {
		if email, ok, err := p.AuthStatus(paths.RealClaudeHome(), true); err == nil && ok {
			nativeClaude = email
		}
	}

	existing := map[string]state.Account{}
	if accounts, err := a.st.ListAccounts(""); err == nil {
		for _, acc := range accounts {
			if acc.QuotaID != "" {
				existing[acc.QuotaID] = acc
			}
		}
	}

	var needLogin []string
	for _, qa := range qcfg.Accounts {
		if !wanted(qa.ID) || (qa.Provider != "claude" && qa.Provider != "codex") {
			continue
		}
		acc, known := existing[qa.ID]
		if !known {
			name := names[qa.ID]
			if name == "" {
				name = defaultName(qa.ID, qa.Provider)
			}
			if err := validName(name); err != nil {
				return err
			}
			id := state.AccountID(qa.Provider, name)
			if _, err := a.st.GetAccount(id); err == nil {
				return fmt.Errorf("%s already exists; pick another name with %s=<name>", id, qa.ID)
			}
			acc = state.Account{
				ID: id, Provider: qa.Provider, Name: name, Enabled: true,
				Home: homeFor(qa.Provider, name), QuotaID: qa.ID, Identity: identity[qa.ID],
				Priority: 100, CreatedAt: time.Now().Unix(),
			}
			if qa.Provider == "claude" && nativeClaude != "" && strings.EqualFold(nativeClaude, identity[qa.ID]) {
				acc.Native = true
				acc.Home = paths.RealClaudeHome()
			}
		}
		if qa.Provider == "codex" && !known {
			if err := codex.ImportAuth(qa.Credentials, acc.Home); err != nil {
				return fmt.Errorf("%s: %w", qa.ID, err)
			}
			if err := a.prepareHome(acc); err != nil {
				return err
			}
			if err := quota.SetCredentials(cfgPath, qa.ID, codex.AuthPath(acc.Home)); err != nil {
				return fmt.Errorf("repoint aiquota %s: %w", qa.ID, err)
			}
			fmt.Printf("imported %s from aiquota %s (auth.json moved to %s; aiquota re-pointed)\n", acc.ID, qa.ID, shortHome(acc.Home))
		}
		if qa.Provider == "claude" {
			if !known && !acc.Native {
				if err := a.prepareHome(acc); err != nil {
					return err
				}
			}
			// aiquota's Claude grant is a PKCE grant of exactly the shape aiq
			// polls with: adopt it as the poll grant and share the file.
			if !claude.HasGrant(acc.Home) && qa.Credentials != "" && qa.Credentials != "keychain" {
				if err := fsutil.CopyFileAtomic(qa.Credentials, claude.GrantPath(acc.Home)); err != nil {
					return fmt.Errorf("%s: copy grant: %w", qa.ID, err)
				}
				if err := quota.SetCredentials(cfgPath, qa.ID, claude.GrantPath(acc.Home)); err != nil {
					return fmt.Errorf("repoint aiquota %s: %w", qa.ID, err)
				}
				fmt.Printf("%s: poll grant adopted from aiquota %s (aiquota re-pointed)\n", acc.ID, qa.ID)
			}
			if !known {
				if acc.Native {
					fmt.Printf("imported %s from aiquota %s as the native ~/.claude login (%s)\n", acc.ID, qa.ID, nativeClaude)
				} else {
					needLogin = append(needLogin, acc.ID)
					fmt.Printf("imported %s from aiquota %s (home %s) — needs a login\n", acc.ID, qa.ID, shortHome(acc.Home))
				}
			}
		}
		if !known {
			if err := a.st.AddAccount(acc); err != nil {
				return err
			}
			a.st.LogEvent(acc.Provider, acc.ID, "import", "from aiquota "+qa.ID, time.Now())
		}
		if qa.Label != "" {
			if _, set := a.cfg.Display.Labels[acc.ID]; !set {
				a.cfg.Display.Labels[acc.ID] = qa.Label
			}
		}
		a.appendOrder(acc.ID)
	}
	if err := config.Save(a.cfg); err != nil {
		return err
	}
	if err := a.pollAndReport(); err != nil {
		fmt.Fprintln(os.Stderr, "aiq:", err)
	}
	if len(needLogin) > 0 {
		fmt.Println()
		fmt.Println("Log the remaining Claude accounts in (browser opens; sign out of claude.ai first, or use a private window):")
		for _, id := range needLogin {
			fmt.Printf("  aiq account login %s\n", id)
		}
	}
	return nil
}

func (a *app) accountLink(id, quotaID string) error {
	acc, err := a.st.GetAccount(id)
	if err != nil {
		return err
	}
	cfg, err := quota.LoadConfig(quota.ConfigPath(a.cfg.Aiquota.Config))
	if err != nil {
		return fmt.Errorf("aiquota config: %w", err)
	}
	for _, qa := range cfg.Accounts {
		if qa.ID == quotaID {
			if qa.Provider != acc.Provider {
				return fmt.Errorf("aiquota %s is a %s account, %s is %s", quotaID, qa.Provider, id, acc.Provider)
			}
			acc.QuotaID = quotaID
			if err := a.st.UpdateAccount(acc); err != nil {
				return err
			}
			fmt.Printf("%s linked to aiquota %s\n", id, quotaID)
			return nil
		}
	}
	return fmt.Errorf("aiquota has no account %q", quotaID)
}

// defaultName strips a provider prefix from an aiquota id: "codex-work" →
// "work", "claude-personal" → "personal", "codex1" → "codex1".
func defaultName(quotaID, provider string) string {
	name := strings.TrimPrefix(quotaID, provider+"-")
	if name == "" {
		return quotaID
	}
	return name
}
