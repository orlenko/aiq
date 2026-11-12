package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/asks"
	"github.com/orlenko/aiq/internal/filelock"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
)

// The list and the key stay apart from aiq's own files, so dropping the feature is deleting them.
const (
	asksDBName  = "asks.db"
	asksKeyName = "typesafe.key"
)

// asksCmd is how to run this feature, as typed, so advice names the binary actually in use.
var asksCmd = filepath.Base(os.Args[0]) + " asks"

func asksKeyPath() string { return filepath.Join(filepath.Dir(paths.ConfigFile()), asksKeyName) }

// asksKey is the model's key. Anything but a regular file only its owner can read counts as
// no key, and with no key nothing leaves the machine.
func asksKey() string {
	info, err := os.Lstat(asksKeyPath())
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return ""
	}
	b, err := os.ReadFile(asksKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// openAsks opens the registry, and aiq's leases read only.
func openAsks() (reg, leases *state.Store, err error) {
	if err := paths.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(paths.StateDB()); err != nil {
		return nil, nil, errors.New("aiq has no state on this machine yet")
	}
	reg, err = state.Open(filepath.Join(paths.DataDir(), asksDBName))
	if err != nil {
		return nil, nil, err
	}
	if err := reg.EnsureAsks(); err != nil {
		reg.Close()
		return nil, nil, err
	}
	leases, err = state.OpenReadOnly(paths.StateDB())
	if err != nil {
		reg.Close()
		return nil, nil, err
	}
	return reg, leases, nil
}

func cmdAsks(args []string) error {
	return cmdAsksWithClock(args, time.Now)
}

// cmdAsksWithClock reads the list's clock only when its output is collected.
func cmdAsksWithClock(args []string, now func() time.Time) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "setup":
		return asksSetup()
	case "dismiss", "restore":
		return asksSetState(sub, args)
	case "list":
		return asksList(args, now)
	case "watch":
		switch {
		case len(args) == 0:
			return asksWatch(false)
		case len(args) == 1 && args[0] == "--service":
			return asksWatch(true)
		}
		return fmt.Errorf("usage: %s watch", asksCmd)
	case "service":
		return asksServiceCmd(args)
	case "reset":
		return asksResetCmd(args)
	case "workspaces":
		return asksWorkspaces()
	case "off", "on":
		return asksOffOn(sub, args)
	case "", "--no-mouse", "--here", "--all":
		return asksApp()
	case "help", "--help", "-h":
		fmt.Print(strings.ReplaceAll(asksUsage, "aiq asks", asksCmd))
		return nil
	}
	return fmt.Errorf("%s", strings.TrimSpace(strings.ReplaceAll(asksUsage, "aiq asks", asksCmd)))
}

const asksUsage = `usage: aiq asks [--all] [--no-mouse]   the side app, on this directory's asks; on every
                                       directory's with --all or when no long session ran here;
                                       the list when not on a terminal
       aiq asks list [--all] [--here]  the list
       aiq asks dismiss ID    dismiss an item
       aiq asks restore ID    put an item back to open
       aiq asks watch         read long sessions and score them, in its own window
       aiq asks service install|uninstall|status  run the watcher in the background instead
       aiq asks reset [--all] [--yes]  erase this directory's asks, or every ask
       aiq asks workspaces    list long sessions' workspaces, on or off
       aiq asks off PATH      send nothing from a workspace; aiq asks on PATH undoes it
       aiq asks setup         store the TypeSafe key
`

func asksSetup() error {
	// Windows reports every file as readable by others, so asksKey would refuse the key
	// this saves.
	if goos == "windows" {
		return errors.New("aiq asks setup is not supported on Windows yet")
	}
	fmt.Fprint(os.Stderr, `aiq asks sends text from long sessions on this machine to api.typesafe.ai, redacted:
everything the agents finish a stretch with, and what you type, from when the watcher starts.
Nothing older is read. It stops for any workspace you turn off.

Paste the TypeSafe API key, then press Enter: `)
	var line string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return errors.New("could not read the key")
		}
		line = string(b)
	} else {
		// A pipe can deliver the key in pieces: read to the end of the line.
		b, err := bufio.NewReader(io.LimitReader(os.Stdin, 64<<10)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return errors.New("could not read the key")
		}
		line = b
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return errors.New("no key entered")
	}
	if err := fsutil.WriteFileAtomic(asksKeyPath(), []byte(key+"\n")); err != nil {
		return err
	}
	if err := os.Chmod(asksKeyPath(), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Saved. Turn off any workspace you do not want sent ("+asksCmd+" workspaces, then off PATH), then start the watcher: "+asksCmd+" service install, or "+asksCmd+" watch in its own window")
	return nil
}

func asksSetState(verb string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: aiq asks %s ID", verb)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("not an item number: %q", sanitize(args[0]))
	}
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	to := state.AskDismissed
	if verb == "restore" {
		to = state.AskOpen
	}
	if err := reg.SetAskByPerson(id, to, time.Now().Unix()); err != nil {
		return err
	}
	a, err := reg.GetAsk(id)
	if err != nil {
		return err
	}
	undo := "restore"
	if verb == "restore" {
		undo = "dismiss"
	}
	fmt.Printf("#%d %s: %s\nUndo: %s %s %d\n", id, to, asksCut(a.FirstQuote, 60), asksCmd, undo, id)
	return nil
}

// asksWorkspaceNames lists the workspaces of this machine's long sessions, as the watcher names them.
func asksWorkspaceNames(leases *state.Store) ([]string, error) {
	list, err := leases.ListLeases()
	if err != nil {
		return nil, err
	}
	host := hostname()
	seen := map[string]bool{}
	out := []string{}
	for _, l := range list {
		name := l.Workspace
		if name == "" {
			name = l.Cwd
		}
		if l.Mode == state.ModeLong && l.Hostname == host && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func asksWorkspaces() error {
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	names, err := asksWorkspaceNames(leases)
	if err != nil {
		return err
	}
	off, err := reg.AskWorkspacesOff()
	if err != nil {
		return err
	}
	for _, name := range names {
		word := "on "
		if off[name] {
			word = "off"
		}
		fmt.Printf("%s  %s\n", word, sanitize(name))
	}
	for name := range off {
		if !containsString(names, name) {
			fmt.Printf("off  %s\n", sanitize(name))
		}
	}
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func asksOffOn(verb string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: aiq asks %s PATH", verb)
	}
	names, err := asksWorkspaceKeys(args[0])
	if err != nil {
		return err
	}
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	for _, name := range names {
		if err := reg.SetAskWorkspaceOff(name, verb == "off", time.Now()); err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", verb, sanitize(name))
	}
	return nil
}

// asksWorkspaceKeys is every name a lease could give the workspace at path: the path as
// typed, with its links resolved, and the root of the git checkout holding it, which is
// what aiq records for a session started inside one.
func asksWorkspaceKeys(path string) ([]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	names := []string{abs}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		names = append(names, real)
	}
	if out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			names = append(names, filepath.Clean(root))
		}
	}
	out := []string{}
	for _, n := range names {
		if !containsString(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// asksReset erases a directory's asks, or every ask when dir is empty.
func asksReset(reg *state.Store, dir string, now time.Time) (int, error) {
	// Fence first, so a session first seen while the reset runs starts from it too. A reset
	// covers the directory, what is under it and every workspace holding it, by either name
	// for it, the same as asksInDir.
	if err := asksFence(reg, dir, now); err != nil {
		return 0, err
	}
	return reg.ResetAsks(func(workspace string) bool { return dir == "" || asksInDir(workspace, dir, asksRealPath) }, now)
}

func asksFence(reg *state.Store, dir string, now time.Time) error {
	if dir == "" {
		return reg.FenceAsks("", now)
	}
	for _, d := range []string{dir, asksRealPath(dir)} {
		if err := reg.FenceAsks(d, now); err != nil {
			return err
		}
		for p := filepath.Dir(d); ; p = filepath.Dir(p) {
			if err := reg.FenceAskWorkspace(p, now); err != nil {
				return err
			}
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	return nil
}

func asksResetCmd(args []string) error {
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	dir, scope := "", "for every workspace"
	if !hasFlag(args, "--all") {
		if dir, err = asksHereDir(); err != nil {
			return err
		}
		scope = "for " + sanitize(dir)
	}
	if !hasFlag(args, "--yes") {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("reset needs --yes when not on a terminal")
		}
		fmt.Printf("Erase all asks %s? This cannot be undone. Type y: ", scope)
		var answer string
		fmt.Scanln(&answer)
		if strings.ToLower(strings.TrimSpace(answer)) != "y" {
			fmt.Println("Kept.")
			return nil
		}
	}
	n, err := asksReset(reg, dir, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("Erased %d asks %s. New asks start from now.\n", n, scope)
	return nil
}

func asksCut(s string, n int) string {
	s = strings.Join(strings.Fields(sanitize(s)), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func asksList(args []string, now func() time.Time) error {
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	here := ""
	if hasFlag(args, "--here") {
		if here, err = asksHereDir(); err != nil {
			return err
		}
	}
	return printAsksListAt(reg, hasFlag(args, "--all"), here, now())
}

// asksHereDir is the directory --here and the f key filter on. It fails rather than return
// nothing, since no directory means every directory.
func asksHereDir() (string, error) {
	d, err := os.Getwd()
	if err == nil && d != "" {
		_, err = os.Stat(d) // macOS still names a directory that was removed
	}
	if err != nil || d == "" {
		return "", errors.New("cannot tell the current directory; use --all for every workspace")
	}
	return d, nil
}

// asksInDir says an item belongs with a directory: its workspace is that directory, holds
// it, or sits under it, by the paths as written or with their links resolved by realPath,
// since a lease records a checkout's real root while a shell may name it through a link.
func asksInDir(workspace, dir string, realPath func(string) string) bool {
	if workspace == "" || dir == "" {
		return false
	}
	under := func(a, b string) bool { return strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") }
	match := func(ws, d string) bool { return ws == d || under(d, ws) || under(ws, d) }
	return match(workspace, dir) || match(realPath(workspace), realPath(dir))
}

// asksRealPaths remembers resolved paths for the life of the process.
var asksRealPaths sync.Map

func asksRealPath(p string) string {
	if v, ok := asksRealPaths.Load(p); ok {
		return v.(string)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		real = p
	}
	asksRealPaths.Store(p, real)
	return real
}

// asksFilter keeps the items that belong with a directory, as asksInDir says with
// realPath. No directory keeps all of them.
func asksFilter(items []state.Ask, dir string, realPath func(string) string) []state.Ask {
	if dir == "" {
		return items
	}
	out := []state.Ask{}
	for _, it := range items {
		if asksInDir(it.Workspace, dir, realPath) {
			out = append(out, it)
		}
	}
	return out
}

func printAsksList(reg *state.Store, all bool, here string) error {
	return printAsksListAt(reg, all, here, time.Now())
}

func printAsksListAt(reg *state.Store, all bool, here string, now time.Time) error {
	items, err := reg.ListAsks("")
	if err != nil {
		return err
	}
	shown := asksFilter(items, here, asksRealPath)
	hidden := len(items) - len(shown)
	items = shown
	st, err := reg.GetAsksStatus(now)
	if err != nil {
		return err
	}
	moved, err := reg.ListAsksMoved(asksDayStart(now))
	if err != nil {
		return err
	}
	if here != "" {
		fmt.Printf("Only %s. %d others hidden, all states.\n", sanitize(here), hidden)
		moved = asksFilter(moved, here, asksRealPath)
	}
	fmt.Print(renderAsksList(items, moved, st, asksKey() != "", all, now, asksCmd))
	return nil
}

// asksDayStart is the start of the local day, for what moved today.
func asksDayStart(now time.Time) int64 {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix()
}

// asksStaleAfter is how long since a pass counts as nothing watching.
const asksStaleAfter = 5 * time.Minute

// asksStatusLine says plainly whether an empty list can be trusted, its advice naming the
// feature's command cmd. The side app and the list both use it, so they never disagree.
func asksStatusLine(st state.AsksStatus, keyOK bool, open int, now time.Time, cmd string) string {
	clock := func(t int64) string { return time.Unix(t, 0).Format("15:04") }
	scope := "long sessions, this machine"
	switch {
	case !keyOK:
		return "Off: no key. Run " + cmd + " setup"
	case st.PassAt == 0:
		return "Not watching. Run " + cmd + " service install, or " + cmd + " watch in its own window"
	case st.LastError != "" && st.NextTryAt > now.Unix():
		return "Failing since " + clock(st.ErrorAt) + ": " + sanitize(st.LastError) + ". Next try " + clock(st.NextTryAt)
	case now.Sub(time.Unix(st.PassAt, 0)) > asksStaleAfter:
		return "Not watching since " + clock(st.PassAt) + ". Run " + cmd + " service install, or " + cmd + " watch"
	case st.LastError != "":
		return "Failing since " + clock(st.ErrorAt) + ": " + sanitize(st.LastError)
	case st.Waiting > 0:
		return fmt.Sprintf("Reading: %d texts waiting. %d watched", st.Waiting, st.Watched)
	case open == 0:
		return fmt.Sprintf("Nothing open. Watching %d, %s ago (%s)", st.Watched, fmtAge(now.Sub(time.Unix(st.PassAt, 0))), scope)
	}
	return fmt.Sprintf("Watching %d, %s ago (%s)", st.Watched, fmtAge(now.Sub(time.Unix(st.PassAt, 0))), scope)
}

func fmtAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}

// asksGroup is an open item's section.
func asksGroup(it state.Ask, now int64) string {
	switch {
	case it.Risk >= .8 && it.AsksReader < .6:
		return "Warnings"
	case now-it.RaisedAt <= 48*3600 && it.AsksReader >= .6:
		return "Asks"
	}
	return "Other open"
}

// renderAsksList is aiq asks list, its advice naming the feature's command cmd.
func renderAsksList(items, moved []state.Ask, st state.AsksStatus, keyOK, all bool, now time.Time, cmd string) string {
	groups := map[string][]state.Ask{}
	open := 0
	var done, shut []state.Ask
	for _, it := range items {
		switch it.State {
		case state.AskOpen:
			g := asksGroup(it, now.Unix())
			groups[g] = append(groups[g], it)
			open++
		case state.AskProbablyDone:
			done = append(done, it)
		default:
			shut = append(shut, it)
		}
	}
	var b strings.Builder
	fmt.Fprintln(&b, asksStatusLine(st, keyOK, open, now, cmd))
	if st.Skipped > 0 {
		fmt.Fprintf(&b, "%d texts could not be read\n", st.Skipped)
	}
	for _, nw := range st.NotWatched {
		fmt.Fprintln(&b, "Not watched: "+sanitize(nw))
	}
	closed, prob := 0, 0
	for _, it := range moved {
		if it.State == state.AskClosed {
			closed++
		} else {
			prob++
		}
	}
	fmt.Fprintf(&b, "Moved today: %d closed, %d probably done\n", closed, prob)
	for _, it := range moved {
		fmt.Fprintf(&b, "  #%d %s, by: %s\n     %s\n", it.ID, it.State, asksCut(it.StateText, 100), asksCut(it.FirstQuote, 100))
	}
	section := func(name string, list []state.Ask) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s\n", name)
		for _, it := range list {
			fmt.Fprintf(&b, "  #%d  %s  %s  %s ago", it.ID, sanitize(filepath.Base(it.Workspace)), sanitize(it.Provider), fmtAge(now.Sub(time.Unix(it.RaisedAt, 0))))
			if it.Repeats > 0 {
				fmt.Fprintf(&b, "  asked %dx", it.Repeats+1)
			}
			fmt.Fprintln(&b)
			for _, line := range strings.Split(sanitize(it.FirstQuote), "\n") {
				fmt.Fprintf(&b, "      > %s\n", line)
			}
			if it.State != state.AskOpen {
				fmt.Fprintf(&b, "      %s", it.State)
				if it.StateReason != "" {
					fmt.Fprintf(&b, " (%s)", sanitize(it.StateReason))
				}
				fmt.Fprintln(&b)
			}
		}
	}
	section("Asks", groups["Asks"])
	section("Warnings", groups["Warnings"])
	section("Other open", groups["Other open"])
	if all {
		section("Probably done", done)
		section("Closed and dismissed", shut)
	} else if len(done)+len(shut) > 0 {
		fmt.Fprintf(&b, "\n%d probably done, %d closed or dismissed: %s list --all\n", len(done), len(shut), cmd)
	}
	return b.String()
}

const asksWatchLock = "asks-watch.lock"

// errAsksWatched means another process holds the watcher's place.
var errAsksWatched = errors.New("another process is already watching")

// holdAsksWatch takes the one watcher's place on this machine.
func holdAsksWatch() (*os.File, error) {
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.DataDir(), asksWatchLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := filelock.TryExclusive(f); err != nil {
		f.Close()
		if filelock.Busy(err) {
			return nil, errAsksWatched
		}
		return nil, err
	}
	return f, nil
}

// asksWatch runs the watcher. As a service it waits for the key and for another watcher
// to stop, instead of exiting into a restart loop.
func asksWatch(service bool) error {
	if !service && asksKey() == "" {
		return errors.New("no key: run " + asksCmd + " setup")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	}
	var lock *os.File
	var err error
	if service {
		lock, err = waitAsksWatch(ctx, asksServiceWait, logf)
	} else {
		lock, err = holdAsksWatch()
	}
	if err != nil || lock == nil {
		return err
	}
	defer lock.Close()
	reg, leases, err := openAsks()
	if err != nil {
		return err
	}
	defer reg.Close()
	defer leases.Close()
	host := hostname()
	// The list starts now. Older text stays unread, here and across a restart.
	start := time.Now()
	if err := reg.RestartAskSessions(start); err != nil {
		return err
	}
	if service {
		logf("watching long sessions on this machine")
	} else {
		fmt.Fprintln(os.Stderr, "Watching long sessions on this machine. Ctrl-C stops it.")
	}
	key, keyGone := "", false
	var w *asks.Watcher
	const pause = 30 * time.Second
	wait := pause
	lastErr := ""
	// Sleep first: a pass that dies at once must not spin through restarts.
	first := time.After(time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-first:
		case <-time.After(wait):
		}
		first = nil
		k := asksKey()
		if k == "" {
			// Said once: a service can wait here for a key for days.
			if !keyGone {
				logf("the key file is gone or not private: nothing is being sent")
				keyGone = true
			}
			wait = pause
			continue
		}
		keyGone = false
		if k != key {
			key = k
			counted := &asks.CountingModel{Inner: &asksKeyGuard{inner: newAsksModel(key), key: key}, St: reg}
			w = &asks.Watcher{Asks: reg, Leases: leases, Scorer: newAsksScorer(reg, counted), Start: start, Hostname: host,
				KeyOK: func() bool { return asksKey() == key }, Logf: logf}
			wait = pause
		}
		res := w.Pass(ctx)
		switch {
		case res.Err == nil:
			wait = pause
			lastErr = ""
		case res.RetryAfter > 0:
			wait = res.RetryAfter
		default:
			wait = min(wait*2, 15*time.Minute)
			asksNoteNextTry(reg, time.Now().Add(wait))
		}
		if res.Err != nil && res.Err.Error() != lastErr {
			lastErr = res.Err.Error()
			logf("%s", lastErr)
		}
	}
}

// asksServiceWait is how often a service watcher tries again for the watcher's place.
var asksServiceWait = 30 * time.Second

// waitAsksWatch takes the watcher's place once no other process holds it. It says so once
// while it waits, and returns a nil file when stopped first.
func waitAsksWatch(ctx context.Context, every time.Duration, logf func(string, ...any)) (*os.File, error) {
	said := false
	for {
		lock, err := holdAsksWatch()
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, errAsksWatched) {
			return nil, err
		}
		if !said {
			logf("another watcher is running, in a window or elsewhere; this one starts when it stops")
			said = true
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(every):
		}
	}
}

// asksNoteNextTry records when a failing watcher tries again, so a long backoff reads as
// failing rather than as no watcher.
func asksNoteNextTry(reg *state.Store, at time.Time) {
	st, err := reg.GetAsksStatus(time.Now())
	if err != nil || st.LastError == "" {
		return
	}
	st.NextTryAt = at.Unix()
	reg.SetAsksStatus(st)
}

// asksKeyGuard sends a request only while the key it was made with is still the key on
// disk, so removing the key or opening its file stops the very next request.
type asksKeyGuard struct {
	inner asks.Model
	key   string
}

func (g *asksKeyGuard) Ask(ctx context.Context, text string, q map[string]asks.Question) (map[string]float64, error) {
	if asksKey() != g.key {
		return nil, errors.New("the key file is gone or not private")
	}
	return g.inner.Ask(ctx, text, q)
}
