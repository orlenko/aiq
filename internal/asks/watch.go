package asks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/redact"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

const (
	// maxTranscript is the largest file a pass parses: parsing holds about the file's size in memory.
	maxTranscript = 2 << 30
	// quiet is how long an unfinished transcript must go unwritten before its last reply counts as final.
	quiet = 90 * time.Second
	// stuckTurn is how long a session reporting a running turn may go unwritten before the
	// turn is read anyway, because a turn can end with no hook.
	stuckTurn = 10 * time.Minute
	// maxTries is how many times one entry is tried before it is skipped and counted.
	maxTries = 5
	// reportEvery is how often a running pass writes the status row.
	reportEvery = 15 * time.Second
	// maxSkipsInRow stops a pass, because many entries rejected in a row means the request is wrong, not the entries.
	maxSkipsInRow = 3
)

// Watcher scores the finished parts of this machine's long sessions.
type Watcher struct {
	Asks     *state.Store // the registry
	Leases   *state.Store // aiq's leases, read only
	Scorer   Scorer
	Start    time.Time // text written before this is never read; zero reads everything
	Hostname string
	KeyOK    func() bool // says the key is still there, checked before each entry
	Now      func() time.Time
	Logf     func(string, ...any)
	// stamps holds each transcript's size and mtime at its last clean read.
	stamps map[string]stamp
	// reported is when a running pass last wrote the status row.
	reported time.Time
	left     int // texts still to score in this pass, for the status row
	prepared int // texts redacted for scoring, which a long backlog must not multiply
}

type stamp struct {
	size  int64
	mtime time.Time
}

// Result is how a pass ended.
type Result struct {
	Err        error
	RetryAfter time.Duration
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// work is a session in a pass.
type work struct {
	row     state.AskSession
	live    bool
	inTurn  bool
	turns   []transcript.Turn
	session *transcript.Session
	mod     time.Time
	size    int64
	failed  bool
	off     bool // turned off during the pass
	grew    bool // written after it was read, so its newest text is not final yet
}

// Pass reads every session and scores what is waiting, one entry per session per round so
// no session starves another. It reports through the status row and returns how it ended.
func (w *Watcher) Pass(ctx context.Context) Result {
	if w.stamps == nil {
		w.stamps = map[string]stamp{}
	}
	st := state.AsksStatus{PassAt: w.now().Unix()}
	if prev, err := w.Asks.GetAsksStatus(w.now()); err == nil {
		st.OkAt, st.ErrorAt, st.LastError = prev.OkAt, prev.ErrorAt, prev.LastError
	}
	// A first pass can run for many minutes, so report at its start and while it runs, or a
	// working watcher reads as no watcher.
	if err := w.Asks.SetAsksStatus(st); err != nil {
		return Result{Err: fmt.Errorf("asks: writing status: %w", err)}
	}
	res, works := w.pass(ctx, &st)
	st.Waiting = waiting(w, works)
	st.PassAt = w.now().Unix()
	if res.Err == nil {
		st.OkAt, st.ErrorAt, st.LastError = st.PassAt, 0, ""
		res = Result{}
	} else {
		if st.ErrorAt == 0 || st.LastError != res.Err.Error() {
			st.ErrorAt = st.PassAt
		}
		st.LastError = res.Err.Error()
	}
	if res.RetryAfter > 0 {
		st.NextTryAt = w.now().Add(res.RetryAfter).Unix()
	}
	if err := w.Asks.SetAsksStatus(st); err != nil && res.Err == nil {
		res.Err = fmt.Errorf("asks: writing status: %w", err)
	}
	return res
}

func waiting(w *Watcher, works []*work) int {
	n := 0
	for _, wk := range works {
		for _, st := range w.pending(wk) {
			if !st.skip {
				n++
			}
		}
	}
	return n
}

// heartbeat reports a running pass, at most every reportEvery.
func (w *Watcher) heartbeat(st *state.AsksStatus, works []*work) {
	if st == nil || w.now().Sub(w.reported) < reportEvery {
		return
	}
	w.reported = w.now()
	st.PassAt, st.Waiting = w.now().Unix(), w.left
	w.Asks.SetAsksStatus(*st)
}

func (w *Watcher) pass(ctx context.Context, st *state.AsksStatus) (Result, []*work) {
	leases, err := w.Leases.ListLeases()
	if err != nil {
		return Result{Err: errors.New("cannot read aiq's sessions")}, nil
	}
	off, err := w.Asks.AskWorkspacesOff()
	if err != nil {
		return Result{Err: errors.New("cannot read the registry")}, nil
	}
	live := map[string]state.Lease{}
	for _, l := range leases {
		if l.Mode != state.ModeLong || l.Hostname != w.Hostname {
			continue
		}
		name := l.Workspace
		if name == "" {
			name = l.Cwd
		}
		switch {
		case l.Provider != "claude" && l.Provider != "codex" && l.Provider != "agy":
			st.NotWatched = append(st.NotWatched, filepath.Base(name)+": "+l.Provider+" cannot be read yet")
			continue
		case off[name]:
			st.NotWatched = append(st.NotWatched, filepath.Base(name)+": turned off")
		case l.Transcript == "" || l.SessionID == "":
			st.NotWatched = append(st.NotWatched, filepath.Base(name)+": no transcript yet")
			continue
		}
		// A session seen for the first time starts at the watcher's start, or later if its
		// workspace was turned on or reset since, and no earlier than its lease: a
		// resumed session is known from its launch. A row seen before keeps its place.
		if _, _, err := w.Asks.UpsertAskSession(state.AskSession{Provider: l.Provider, SessionID: l.SessionID,
			Chain: name, Workspace: name, Transcript: l.Transcript, NotBefore: max(w.Start.UnixMilli(), l.StartedAt*1000)}); err != nil {
			return Result{Err: errors.New("cannot write the registry")}, nil
		}
		live[l.Provider+"\x00"+l.SessionID] = l
	}
	rows, err := w.Asks.ListAskSessions()
	if err != nil {
		return Result{Err: errors.New("cannot read the registry")}, nil
	}
	var works []*work
	// waits holds back each chain past where a session in a running turn, not read this pass,
	// may have text still to score.
	waits := map[string]time.Time{}
	for _, row := range rows {
		l, isLive := live[row.Provider+"\x00"+row.SessionID]
		wk, reason, inTurn := w.load(row, isLive, l.InTurn(), off[row.Workspace])
		if inTurn {
			at := time.UnixMilli(max(row.Position.TurnStarted, row.NotBefore, 0))
			if seen, has := waits[row.Chain]; !has || at.Before(seen) {
				waits[row.Chain] = at
			}
		}
		if reason != "" {
			st.NotWatched = append(st.NotWatched, filepath.Base(row.Workspace)+": "+reason)
		}
		if isLive && reason == "" && !off[row.Workspace] {
			st.Watched++
		}
		if wk == nil {
			continue
		}
		if off[row.Workspace] {
			if err := w.skipToEnd(wk); err != nil {
				return Result{Err: errors.New("cannot write the registry")}, works
			}
			continue
		}
		works = append(works, wk)
	}
	res := w.rounds(ctx, works, waits, st)
	for _, wk := range works {
		if _, more := w.next(wk); wk.failed || more || w.held(wk) {
			continue
		}
		w.stamps[wk.row.Transcript] = stamp{wk.size, wk.mod}
		if !wk.live && w.now().Sub(wk.mod) >= quiet {
			if err := w.Asks.SetAskSessionDone(wk.row.Provider, wk.row.SessionID); err != nil && res.Err == nil {
				res.Err = errors.New("cannot write the registry")
			}
		}
	}
	return res, works
}

// load reads a session's file if it changed since its last clean read. A nil work with no
// reason means nothing to do this pass; inTurn says that is because a turn is running.
// A turned-off session is read even mid-turn, so its place moves past what it writes.
func (w *Watcher) load(row state.AskSession, live, inTurn, isOff bool) (*work, string, bool) {
	info, err := os.Lstat(row.Transcript)
	if err != nil {
		if !live {
			w.Asks.SetAskSessionDone(row.Provider, row.SessionID)
		}
		return nil, "", false
	}
	if !info.Mode().IsRegular() {
		return nil, "its transcript is not a regular file", false
	}
	if info.Size() > maxTranscript {
		return nil, "its transcript is over 2 GB", false
	}
	if inTurn && !isOff && w.now().Sub(info.ModTime()) < stuckTurn {
		return nil, "", true
	}
	if w.stamps[row.Transcript] == (stamp{info.Size(), info.ModTime()}) {
		// Read to its end already: once it has ended and gone quiet, stop watching it.
		if !live && w.now().Sub(info.ModTime()) >= quiet {
			if err := w.Asks.SetAskSessionDone(row.Provider, row.SessionID); err != nil {
				return nil, "the registry cannot be written", false
			}
		}
		return nil, "", false
	}
	sess, err := parse(row.Provider, row.Transcript)
	if err != nil {
		return nil, "its transcript cannot be read", false
	}
	if sess == nil || sess.Worker {
		w.stamps[row.Transcript] = stamp{info.Size(), info.ModTime()}
		return nil, "", false
	}
	return &work{row: row, live: live, inTurn: inTurn, session: sess, turns: sess.Turns, mod: info.ModTime(), size: info.Size()}, "", false
}

func parse(provider, path string) (*transcript.Session, error) {
	switch provider {
	case "claude":
		return transcript.ParseClaude(path)
	case "codex":
		return transcript.ParseCodex(path)
	case "agy":
		return transcript.ParseAgy(path)
	}
	return nil, nil
}

func started(t transcript.Turn) int64 {
	if !t.Started.IsZero() {
		return t.Started.UnixMilli()
	}
	if !t.Ended.IsZero() {
		return t.Ended.UnixMilli()
	}
	return 0
}

// skipToEnd moves a turned-off session past everything written, so nothing from the time it
// was off is sent when it is turned back on.
func (w *Watcher) skipToEnd(wk *work) error {
	if len(wk.turns) == 0 {
		return nil
	}
	last := wk.turns[len(wk.turns)-1]
	to := state.AskPosition{TurnStarted: started(last), ItemsDone: len(entriesOf(last)), ReplySHA: sha(last.Reply), Gen: wk.row.Position.Gen}
	if to == wk.row.Position {
		return nil
	}
	err := w.Asks.ApplyAskPlan(wk.row.Provider, wk.row.SessionID, wk.row.Position, to, 0, state.AskPlan{})
	if err == nil {
		wk.row.Position = to
	}
	return err
}

// held says the newest text may still change.
func (w *Watcher) held(wk *work) bool {
	if len(wk.turns) == 0 || !(wk.session != nil && wk.session.Unfinished || wk.turns[len(wk.turns)-1].Open || wk.row.Provider == "agy") {
		return false
	}
	if wk.grew || w.now().Sub(wk.mod) < quiet {
		return true
	}
	// A long pass can outlast the quiet time while the file is still being written. What
	// was read then is a draft; the next pass reads the file again.
	info, err := os.Lstat(wk.row.Transcript)
	if err != nil || info.Size() != wk.size || !info.ModTime().Equal(wk.mod) {
		wk.grew = true
		return true
	}
	return false
}

// raw is one scoreable thing in a turn. A nil Entry is a place in the turn with nothing to score.
type raw struct {
	kind EntryKind
	text string
	at   time.Time
	skip bool
	// final says this is t.Reply, kept apart because its text can change in place.
	final bool
}

// entriesOf lists a turn's texts by index. The prompt is index 0, and Items follow, because
// Items only grow as a file does. Readers that keep no items use Steers and Earlier.
func entriesOf(t transcript.Turn) []raw {
	// A prompt too long to read is no reply to score.
	out := []raw{{kind: EntryReply, text: t.Prompt, at: t.Started, skip: t.Source != transcript.Human || t.PromptSkipped}}
	if len(t.Items) > 0 {
		for _, it := range t.Items {
			switch it.Kind {
			case transcript.ItemSteer:
				out = append(out, raw{kind: EntryReply, text: it.Text, at: it.At})
			case transcript.ItemAnswer:
				if it.Provisional {
					return out
				}
				// An answer that replaces the one before it leaves that one unread.
				if it.Supersedes && len(out) > 0 && out[len(out)-1].kind == EntryAnswer {
					out[len(out)-1].skip = true
				}
				out = append(out, raw{kind: EntryAnswer, text: it.Text, at: it.At})
			default:
				out = append(out, raw{skip: true})
			}
		}
		return out
	}
	for _, s := range t.Steers {
		out = append(out, raw{kind: EntryReply, text: s, at: t.Started})
	}
	for _, s := range t.Earlier {
		out = append(out, raw{kind: EntryAnswer, text: s, at: t.Started})
	}
	return out
}

func sha(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// step is one entry to score and where the session stands after it.
type step struct {
	entry Entry
	skip  bool
	to    state.AskPosition
	at    time.Time
	held  bool // an answer that may still change, handed out only to mark its place in the chain
}

// pending lists what is left to score in a session, in order, without preparing the texts.
// A turn's reply comes last, and only when its text differs from what was scored and no
// answer item already had it.
func (w *Watcher) pending(wk *work) []step {
	var out []step
	w.walk(wk, false, false, func(st step) bool {
		out = append(out, st)
		return true
	})
	return out
}

// walk hands each step of pending to yield, in order, until yield returns false, so finding
// the next entry does not prepare the whole backlog. Only with prepare is a step's text
// redacted into its entry. With barrier, an answer still held is handed out last, marked
// held, so the chain waits for it.
func (w *Watcher) walk(wk *work, prepare, barrier bool, yield func(step) bool) {
	pos := wk.row.Position
	held := w.held(wk)
	for i, t := range wk.turns {
		s := started(t)
		if s == 0 || s < pos.TurnStarted {
			continue
		}
		list := entriesOf(t)
		from, replySHA := 0, ""
		if s == pos.TurnStarted {
			from, replySHA = pos.ItemsDone, pos.ReplySHA
		}
		last := i == len(wk.turns)-1
		aiq := t.Source == transcript.Aiq
		for k := from; k < len(list); k++ {
			r := list[k]
			// An answer scored as the turn's reply can become an item later, once its
			// stretch closes. It was already scored.
			if r.kind == EntryAnswer && !r.skip && replySHA != "" && sha(r.text) == replySHA {
				r.skip = true
			}
			if last && held && k == len(list)-1 && r.kind == EntryAnswer && !r.skip {
				if barrier {
					yield(step{held: true, at: r.at})
				}
				return
			}
			if !yield(w.stepFor(wk, r, aiq, prepare, state.AskPosition{TurnStarted: s, ItemsDone: k + 1, ReplySHA: replySHA, Gen: pos.Gen})) {
				return
			}
		}
		if t.Reply != "" && sha(t.Reply) != replySHA && !hasAnswer(list, t.Reply) {
			at := t.Ended
			if at.IsZero() {
				at = t.Started
			}
			// Ended is the turn's last write of any kind, not when the reply was written, so a
			// turn that started before the cutoff may hold a reply from before it. An answer
			// item with the reply's text, such as a provisional one, says when it was written.
			old := wk.row.NotBefore > 0 && s < wk.row.NotBefore
			written, known := replyWritten(t)
			if known {
				at, old = written, wk.row.NotBefore > 0 && written.UnixMilli() < wk.row.NotBefore
			}
			if last && (held || t.Open) {
				if barrier {
					// Held, it marks its place from when it may have been written: the turn's
					// start, unless the transcript says when.
					if !known {
						at = t.Started
					}
					yield(step{held: true, at: at})
				}
				return
			}
			if !yield(w.stepFor(wk, raw{kind: EntryAnswer, text: t.Reply, at: at, skip: old}, aiq, prepare,
				state.AskPosition{TurnStarted: s, ItemsDone: len(list), ReplySHA: sha(t.Reply), Gen: pos.Gen})) {
				return
			}
		}
	}
}

// replyWritten is when the turn's reply was written, from an answer item with its text, or
// from the parser where the transcript has no items.
func replyWritten(t transcript.Turn) (time.Time, bool) {
	for i := len(t.Items) - 1; i >= 0; i-- {
		if it := t.Items[i]; it.Kind == transcript.ItemAnswer && it.Text == t.Reply && !it.At.IsZero() {
			return it.At, true
		}
	}
	return t.ReplyAt, !t.ReplyAt.IsZero()
}

func hasAnswer(list []raw, text string) bool {
	for _, r := range list {
		if r.kind == EntryAnswer && !r.skip && r.text == text {
			return true
		}
	}
	return false
}

func (w *Watcher) stepFor(wk *work, r raw, aiq, prepare bool, to state.AskPosition) step {
	st := step{to: to, skip: r.skip, at: r.at}
	if r.skip {
		return st
	}
	// Older than the watcher's start or the person's last reset: it stays unread.
	if wk.row.NotBefore > 0 && (r.at.IsZero() || r.at.UnixMilli() < wk.row.NotBefore) {
		st.skip = true
		return st
	}
	text := r.text
	// An error the CLI wrote, such as a lost connection, is not the agent asking anything.
	if r.kind == EntryAnswer && strings.HasPrefix(strings.TrimSpace(text), "API Error:") {
		st.skip = true
		return st
	}
	if r.kind == EntryReply {
		var ok bool
		if text, ok = replyText(text); !ok {
			st.skip = true
			return st
		}
	}
	if prepare {
		w.prepared++
		st.entry = Entry{Meta: Meta{Chain: wk.row.Chain, Workspace: wk.row.Workspace, Provider: wk.row.Provider, SessionID: wk.row.SessionID},
			Kind: r.kind, Text: redact.Clean(text), At: r.at.Unix(), Aiq: aiq && r.kind == EntryAnswer}
	}
	return st
}

// next is a session's next text to score: a run of texts with nothing to score is taken as one.
func (w *Watcher) next(wk *work) (step, bool) {
	var pick step
	found := false
	w.walk(wk, true, false, func(st step) bool {
		if found && !st.skip {
			return false
		}
		pick, found = st, true
		return st.skip
	})
	return pick, found
}

// upcoming is when a session's next text to score was written, counting an answer it holds
// until it is final.
func (w *Watcher) upcoming(wk *work) (time.Time, bool) {
	var at time.Time
	found := false
	w.walk(wk, false, true, func(st step) bool {
		if st.skip {
			return true
		}
		at, found = st.at, true
		return false
	})
	return at, found
}

// rounds scores one entry per session per round until nothing is left or a failure stops it.
// Sessions of one chain go in the order their texts were written, so a reply in a successor
// is not read before the question it answers in the session before it.
func (w *Watcher) rounds(ctx context.Context, works []*work, waits map[string]time.Time, st *state.AsksStatus) Result {
	skips, stalled := 0, false
	w.left = waiting(w, works)
	for {
		w.heartbeat(st, works)
		progress, deferred := false, false
		// A chain goes by each session's next text to score, past any it skips. A session that
		// failed this pass still holds back later texts of its chain until it is tried again.
		first := maps.Clone(waits)
		if first == nil {
			first = map[string]time.Time{}
		}
		for _, wk := range works {
			if wk.off {
				continue
			}
			if at, ok := w.upcoming(wk); ok {
				if seen, has := first[wk.row.Chain]; !has || at.Before(seen) {
					first[wk.row.Chain] = at
				}
			}
		}
		for _, wk := range works {
			if wk.failed {
				continue
			}
			if ctx.Err() != nil {
				return Result{}
			}
			pick, ok := w.next(wk)
			if !ok {
				continue
			}
			if !pick.skip && pick.at.After(first[wk.row.Chain]) {
				deferred = true
				continue // an earlier text of the same chain goes first
			}
			if !pick.skip && w.KeyOK != nil && !w.KeyOK() {
				return Result{Err: errors.New("the key file is gone or not private")}
			}
			if !pick.skip {
				// Turned off during the pass: send nothing more from it.
				off, err := w.Asks.AskWorkspaceOff(wk.row.Workspace)
				if err != nil {
					return Result{Err: errors.New("cannot read the registry")}
				}
				if off {
					wk.failed, wk.off = true, true
					continue
				}
				// Reset or moved by another watcher during the pass: go on from the session as
				// stored, cutoff included, before sending anything.
				row, err := w.Asks.GetAskSession(wk.row.Provider, wk.row.SessionID)
				if err != nil {
					return Result{Err: errors.New("cannot read the registry")}
				}
				if row.Position != wk.row.Position || row.NotBefore != wk.row.NotBefore {
					wk.row = row
					progress = true
					continue
				}
			}
			err := w.score(ctx, wk, pick)
			if err == nil && !pick.skip {
				w.left = max(w.left-1, 0)
			}
			w.heartbeat(st, works)
			if err == nil {
				// Only an answer is sure to have reached the model, so only it ends a run of
				// rejections.
				if !pick.skip && pick.entry.Kind == EntryAnswer {
					skips = 0
				}
				progress = true
				continue
			}
			switch classOf(err) {
			case ClassMalformed:
				wk.failed = true
				if done, e := w.tryAgainOrSkip(wk, pick); e != nil {
					return Result{Err: e}
				} else if done {
					progress = true
				}
			case ClassRejected:
				if e := w.skipEntry(wk, pick); e != nil {
					return Result{Err: e}
				}
				progress = true
				skips++
				if skips >= maxSkipsInRow {
					return Result{Err: err, RetryAfter: 15 * time.Minute}
				}
			case ClassAuth:
				return Result{Err: err, RetryAfter: 15 * time.Minute}
			case ClassRate:
				var me *ModelError
				errors.As(err, &me)
				return Result{Err: err, RetryAfter: me.RetryAfter}
			default:
				return Result{Err: err}
			}
		}
		// A session put off for an earlier one that then failed gets one more round.
		if !progress && (!deferred || stalled) {
			return Result{}
		}
		stalled = !progress
	}
}

// score runs the rules on one step and applies the result. A panic in the rules counts as a
// failure of this entry and logs only where it happened, since its value could hold quoted text.
func (w *Watcher) score(ctx context.Context, wk *work, st step) (err error) {
	defer func() {
		if r := recover(); r != nil {
			_, file, line, _ := runtime.Caller(2)
			w.logf("asks: panic at %s:%d", filepath.Base(file), line)
			err = &ModelError{Class: ClassMalformed}
		}
	}()
	var plan state.AskPlan
	if !st.skip {
		plan, err = w.Scorer.Score(ctx, st.entry)
		if err != nil {
			return err
		}
	}
	err = w.Asks.ApplyAskPlan(wk.row.Provider, wk.row.SessionID, wk.row.Position, st.to, 0, plan)
	if errors.Is(err, state.ErrAskPositionMoved) {
		row, e := w.Asks.GetAskSession(wk.row.Provider, wk.row.SessionID)
		if e == nil {
			wk.row = row
			return nil
		}
	}
	if err != nil {
		return errors.New("cannot write the registry")
	}
	wk.row.Position = st.to
	return nil
}

// skipEntry passes over one entry. If the session moved meanwhile (a reset, or another
// watcher), it takes the session as stored, cutoff included, rather than the place it planned.
func (w *Watcher) skipEntry(wk *work, st step) error {
	err := w.Asks.ApplyAskPlan(wk.row.Provider, wk.row.SessionID, wk.row.Position, st.to, 1, state.AskPlan{})
	if errors.Is(err, state.ErrAskPositionMoved) {
		row, e := w.Asks.GetAskSession(wk.row.Provider, wk.row.SessionID)
		if e != nil {
			return errors.New("cannot read the registry")
		}
		wk.row = row
		return nil
	}
	if err != nil {
		return errors.New("cannot write the registry")
	}
	wk.row.Position = st.to
	return nil
}

// tryAgainOrSkip counts a failure of an entry, and skips it once it has failed too often.
func (w *Watcher) tryAgainOrSkip(wk *work, st step) (bool, error) {
	n, err := w.Asks.AddAskAttempt(wk.row.Provider, wk.row.SessionID)
	if err != nil {
		return false, errors.New("cannot write the registry")
	}
	if n < maxTries {
		return false, nil
	}
	return true, w.skipEntry(wk, st)
}

// CountingModel counts each call before it is sent, so the day's use can be seen. It never
// refuses one, and a count it cannot write does not stop the call.
type CountingModel struct {
	Inner Model
	St    *state.Store
	Now   func() time.Time
}

// Ask counts the call and passes it on.
func (c *CountingModel) Ask(ctx context.Context, st string, q map[string]Question) (map[string]float64, error) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	c.St.CountAskCall(state.AsksDay(now))
	return c.Inner.Ask(ctx, st, q)
}
