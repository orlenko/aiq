package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type appHistoryFailure string

func (e appHistoryFailure) Error() string { return string(e) }

type appHistoryOutput struct {
	Data    any
	Partial bool
	Omitted map[string]int
}
type appHistoryAskReader func(context.Context, int64, string) (state.Ask, error)
type appHistoryDeps struct {
	input   navFactoryInput
	cache   *transcript.Cache
	source  transcript.AllDiscovery
	readAsk appHistoryAskReader
}

// appHistoryRun reads local configured roots. It neither starts a daemon nor
// changes a provider/terminal; one-shot invocations need no warm process cache.
func appHistoryRun(ctx context.Context, kind string, args []string, readAsk appHistoryAskReader) (appHistoryOutput, error) {
	return appHistoryRunWithInput(ctx, navProductionInput(), kind, args, readAsk)
}
func appHistoryRunWithInput(ctx context.Context, in navFactoryInput, kind string, args []string, readAsk appHistoryAskReader) (appHistoryOutput, error) {
	roots, err := in.roots(ctx)
	if err != nil {
		return appHistoryOutput{}, appHistoryFailure("unavailable")
	}
	d := appHistoryDeps{input: in, cache: transcript.NewCache(), source: transcript.NewIndex(roots), readAsk: readAsk}
	return d.run(ctx, roots, kind, args)
}
func (d appHistoryDeps) run(ctx context.Context, roots transcript.Roots, kind string, args []string) (appHistoryOutput, error) {
	opts := map[string]string{}
	for len(args) > 0 {
		if len(args) < 2 || !strings.HasPrefix(args[0], "--") {
			return appHistoryOutput{}, appHistoryFailure("invalid_request")
		}
		key := args[0][2:]
		if _, duplicate := opts[key]; duplicate {
			return appHistoryOutput{}, appHistoryFailure("invalid_request")
		}
		opts[key] = args[1]
		args = args[2:]
	}
	allowed := map[string]bool{}
	switch kind {
	case "sessions":
		allowed = map[string]bool{"limit": true, "before": true}
	case "conversation":
		allowed = map[string]bool{"provider": true, "id": true, "at": true, "before": true, "after": true}
	case "ask-context":
		allowed = map[string]bool{"id": true, "revision": true}
	default:
		return appHistoryOutput{}, appHistoryFailure("invalid_request")
	}
	for key, value := range opts {
		if !allowed[key] || value == "" || len(value) > 512 {
			return appHistoryOutput{}, appHistoryFailure("invalid_request")
		}
	}
	if kind == "sessions" {
		return d.sessions(ctx, roots, opts)
	}
	if kind == "ask-context" {
		return d.askContext(ctx, opts)
	}
	if !transcript.IsFullID(opts["provider"], opts["id"]) || appHistoryOptionCount(opts, "at", "before", "after") > 1 {
		return appHistoryOutput{}, appHistoryFailure("invalid_request")
	}
	var at int64
	if opts["at"] != "" {
		var err error
		at, err = strconv.ParseInt(opts["at"], 10, 64)
		if err != nil || at <= 0 {
			return appHistoryOutput{}, appHistoryFailure("invalid_request")
		}
	}
	s, err := d.session(ctx, opts["provider"], opts["id"])
	if err != nil {
		return appHistoryOutput{}, err
	}
	data, partial, err := appHistoryWindow(ctx, s, at, opts["before"], opts["after"])
	return appHistoryOutput{Data: data, Partial: partial, Omitted: map[string]int{}}, err
}
func appHistoryOptionCount(opts map[string]string, keys ...string) int {
	n := 0
	for _, key := range keys {
		if opts[key] != "" {
			n++
		}
	}
	return n
}
func appHistoryText(raw string, limit int) (string, bool) {
	if len(raw) <= limit {
		return raw, false
	}
	end := limit
	for end > 0 && !utf8.RuneStart(raw[end]) {
		end--
	}
	return raw[:end], true
}
func appHistoryLabel(raw string) string {
	text := []rune(transcript.Clean(raw))
	if len(text) > 280 {
		text = text[:280]
	}
	return string(text)
}
func appHistoryMillis(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}

type appRecentCandidate struct {
	candidate transcript.Candidate
	at        int64
	key       string
}

func appHistoryCandidateKey(c transcript.Candidate) string {
	sum := sha256.Sum256([]byte(c.Provider + "\x00" + c.Path))
	return hex.EncodeToString(sum[:16])
}
func appHistoryRecent(ctx context.Context, candidates []transcript.Candidate, limit int, before string) ([]appRecentCandidate, int, int, error) {
	var beforeAt int64
	var beforeKey string
	if before != "" {
		parts := strings.Split(before, ".")
		if len(parts) != 2 || len(parts[1]) != 32 {
			return nil, 0, 0, appHistoryFailure("invalid_request")
		}
		var err error
		beforeAt, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, 0, 0, appHistoryFailure("invalid_request")
		}
		if _, err = hex.DecodeString(parts[1]); err != nil {
			return nil, 0, 0, appHistoryFailure("invalid_request")
		}
		beforeKey = parts[1]
	}
	rows := make([]appRecentCandidate, 0, len(candidates))
	unreadable := 0
	seen := map[string]bool{}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return nil, 0, unreadable, appHistoryFailure("busy")
		}
		key := appHistoryCandidateKey(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		snap, err := transcript.Stat(c.Path)
		if err != nil {
			unreadable++
			continue
		}
		at := snap.Mtime
		if before != "" && (at > beforeAt || at == beforeAt && key <= beforeKey) {
			continue
		}
		rows = append(rows, appRecentCandidate{c, at, key})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].at != rows[j].at {
			return rows[i].at > rows[j].at
		}
		return rows[i].key < rows[j].key
	})
	total := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total, unreadable, nil
}
func (d appHistoryDeps) sessions(ctx context.Context, roots transcript.Roots, opts map[string]string) (appHistoryOutput, error) {
	limit := 100
	if opts["limit"] != "" {
		n, err := strconv.Atoi(opts["limit"])
		if err != nil || n < 1 || n > 500 {
			return appHistoryOutput{}, appHistoryFailure("invalid_request")
		}
		limit = n
	}
	candidates, err := d.source.AllCandidates(ctx)
	if err != nil {
		return appHistoryOutput{}, appHistoryFailure("unavailable")
	}
	selected, total, unreadable, err := appHistoryRecent(ctx, candidates, limit, opts["before"])
	if err != nil {
		return appHistoryOutput{}, err
	}
	summaries := []transcript.Summary{}
	scanned := 0
	cursor := ""
	byID := map[string]int{}
	parseCtx := ctx
	parseCancel := func() {}
	if deadline, ok := ctx.Deadline(); ok {
		parseCtx, parseCancel = context.WithDeadline(ctx, deadline.Add(-time.Second))
	}
	defer parseCancel()
	for _, candidate := range selected {
		if parseCtx.Err() != nil {
			break
		}
		row, err := d.cache.Summary(parseCtx, candidate.candidate.Path)
		if errors.Is(err, transcript.ErrBudget) {
			break
		}
		scanned++
		cursor = fmt.Sprintf("%d.%s", candidate.at, candidate.key)
		if err != nil {
			unreadable++
			continue
		}
		if row.Empty {
			continue
		}
		if row.Provider != candidate.candidate.Provider || !transcript.IsFullID(row.Provider, row.ID) {
			unreadable++
			continue
		}
		if row.Cwd == "" {
			row.Cwd = candidate.candidate.Directory
		}
		key := row.Provider + "/" + row.ID
		if _, ok := byID[key]; ok {
			continue
		}
		byID[key] = len(summaries)
		summaries = append(summaries, row)
	}
	// Leave a little time for launch metadata and ask counts. A timed-out parse
	// still returns the completed recent window and a cursor to continue it.
	metaCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	f := &navFactory{input: d.input, policy: navProductionPolicy(), askSlots: make(chan struct{}, 1)}
	run := &navListRun{limit: 4 << 20, needed: map[string]struct{}{}, cancel: cancel}
	extras, extraErr := f.listExtras(metaCtx, roots, navListQuery{all: true, workers: true}, summaries, run)
	rows := []map[string]any{}
	for i, s := range summaries {
		r := map[string]any{"kind": "session", "provider": s.Provider, "id": s.ID, "cwd": s.Cwd, "name": appHistoryLabel(s.Label), "title": appHistoryLabel(s.Title), "model": s.Model, "branch": s.Branch, "started_ms": appHistoryMillis(s.Started), "updated_ms": appHistoryMillis(s.Updated), "turns": s.Turns, "tools": s.Tools, "worker": s.Worker, "workspace": "", "workspace_known": false, "pending_asks": 0, "pending_asks_known": false, "live": "unknown", "account": "", "launcher": "", "terminal": nil, "terminal_reason": "unknown"}
		if extraErr == nil {
			r["workspace"] = extras.workspaces[i].path
			r["workspace_known"] = extras.workspaces[i].known
			r["pending_asks"] = extras.asks[i].count
			r["pending_asks_known"] = extras.asks[i].known
			r["live"] = extras.activity[i].Live
			r["account"] = extras.activity[i].Account
			r["launcher"] = extras.activity[i].Launcher
		}
		rows = append(rows, r)
	}
	if extraErr == nil {
		for _, s := range extras.rows {
			started, _ := time.Parse(time.RFC3339Nano, s.Started)
			updated, _ := time.Parse(time.RFC3339Nano, s.Updated)
			rows = append(rows, map[string]any{"kind": "workspace", "provider": s.Provider, "id": "", "cwd": s.Cwd, "name": "", "title": "", "model": "", "branch": "", "started_ms": appHistoryMillis(started), "updated_ms": appHistoryMillis(updated), "turns": 0, "tools": 0, "worker": false, "workspace": s.Workspace, "workspace_known": s.WorkspaceKnown, "pending_asks": s.PendingAsks, "pending_asks_known": s.PendingAsksKnown, "live": s.Activity.Live, "account": s.Activity.Account, "launcher": s.Activity.Launcher, "terminal": nil, "terminal_reason": "no_lease"})
		}
	}
	older := total - scanned
	if older == 0 {
		cursor = ""
	}
	partial := older > 0 || unreadable > 0 || extraErr != nil || extras.partial
	return appHistoryOutput{Data: map[string]any{"window": map[string]any{"limit": limit, "scanned": scanned, "of": total, "cursor": cursor}, "activity": map[string]any{"known": extraErr == nil && extras.summary.Known, "live": extras.summary.Live}, "rows": rows}, Partial: partial, Omitted: map[string]int{"older": older, "unreadable": unreadable}}, nil
}

func (d appHistoryDeps) session(ctx context.Context, provider, id string) (*transcript.Session, error) {
	hit, err := transcript.FindByID(ctx, d.source, provider, id, d.cache.Summary)
	if err != nil {
		if errors.Is(err, transcript.ErrNoSuchSession) {
			return nil, appHistoryFailure("not_found")
		}
		return nil, appHistoryFailure("unavailable")
	}
	s, _, err := d.cache.Session(ctx, hit.Path)
	if err != nil {
		if errors.Is(err, transcript.ErrPreparing) || ctx.Err() != nil {
			return nil, appHistoryFailure("preparing")
		}
		if errors.Is(err, transcript.ErrTooLarge) {
			return nil, appHistoryFailure("too_large")
		}
		return nil, appHistoryFailure("unavailable")
	}
	if s == nil || s.Provider != provider || s.ID != id {
		return nil, appHistoryFailure("not_found")
	}
	return s, nil
}

type appHistoryMessage struct {
	Key       string `json:"key"`
	Turn      int    `json:"turn"`
	Role      string `json:"role"`
	Source    string `json:"source,omitempty"`
	At        int64  `json:"at_ms"`
	Text      string `json:"text"`
	Bytes     int    `json:"bytes"`
	FullBytes int    `json:"full_bytes"`
	Truncated bool   `json:"truncated"`
	Anchor    bool   `json:"anchor,omitempty"`
}

func appHistoryMessages(ctx context.Context, s *transcript.Session) ([]appHistoryMessage, error) {
	return appHistoryMessagesWithQuote(ctx, s, nil)
}
func appHistoryMessagesWithQuote(ctx context.Context, s *transcript.Session, quotes []string) ([]appHistoryMessage, error) {
	out := []appHistoryMessage{}
	add := func(turn int, role, text string, at time.Time) {
		if text == "" {
			return
		}
		part := len(out)
		sum := sha256.Sum256([]byte(text))
		key := fmt.Sprintf("m%d-%d-%x", turn, part, sum[:8])
		short, cut := appHistoryText(text, 256<<10)
		if cut {
			for _, quote := range quotes {
				if len(quote) < 16 || len(quote) > 256<<10 {
					continue
				}
				at := strings.Index(text, quote)
				if at < 0 {
					continue
				}
				start := max(0, at-1024)
				if at+len(quote)-start > 256<<10 {
					start = at
				}
				for start > 0 && !utf8.RuneStart(text[start]) {
					start--
				}
				short, _ = appHistoryText(text[start:], 256<<10)
				break
			}
		}
		out = append(out, appHistoryMessage{Key: key, Turn: turn, Role: role, At: appHistoryMillis(at), Text: short, Bytes: len(short), FullBytes: len(text), Truncated: cut})
	}
	for i, t := range s.Turns {
		if ctx.Err() != nil {
			return nil, appHistoryFailure("busy")
		}
		role, source := "user", ""
		switch t.Source {
		case transcript.Aiq:
			role, source = "system", "aiq"
		case transcript.Peer:
			role, source = "system", "peer"
		case transcript.Notice:
			role, source = "system", "notice"
		}
		before := len(out)
		add(i, role, t.Prompt, t.Started)
		if len(out) > before {
			out[len(out)-1].Source = source
		}
		if len(t.Items) > 0 {
			for _, item := range t.Items {
				role := "assistant"
				if item.Kind == transcript.ItemSteer {
					role = "user"
				}
				if (item.Promotes || item.Supersedes) && len(out) > 0 && out[len(out)-1].Turn == i && out[len(out)-1].Role == "assistant" {
					if item.Promotes && out[len(out)-1].Text == item.Text {
						continue
					}
					if item.Supersedes {
						out = out[:len(out)-1]
					}
				}
				add(i, role, item.Text, item.At)
			}
		} else {
			for _, text := range t.Steers {
				add(i, "user", text, t.Started)
			}
			for _, text := range t.Earlier {
				add(i, "assistant", text, t.Started)
			}
			add(i, "assistant", t.Reply, t.ReplyAt)
		}
	}
	return out, nil
}
func appHistoryWindow(ctx context.Context, s *transcript.Session, at int64, before, after string) (map[string]any, bool, error) {
	all, err := appHistoryMessages(ctx, s)
	if err != nil {
		return nil, false, err
	}
	anchor := -1
	if at > 0 {
		for i, m := range all {
			if m.At == at {
				anchor = i
				break
			}
		}
		if anchor < 0 {
			for i, t := range s.Turns {
				if appHistoryMillis(t.Started) == at {
					for j, m := range all {
						if m.Turn == i {
							anchor = j
							break
						}
					}
					break
				}
			}
		}
		if anchor < 0 {
			return nil, false, appHistoryFailure("not_found")
		}
		all[anchor].Anchor = true
	}
	return appHistorySlice(s, all, anchor, before, after)
}
func appHistorySlice(s *transcript.Session, all []appHistoryMessage, anchor int, before, after string) (map[string]any, bool, error) {
	start, end := 0, len(all)
	if before != "" || after != "" {
		key := before
		if key == "" {
			key = after
		}
		index := -1
		for i, m := range all {
			if m.Key == key {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, false, appHistoryFailure("conflict")
		}
		if before != "" {
			end = index
		} else {
			start = index + 1
		}
	}
	const budget = 3 << 20
	if anchor >= 0 {
		// The requested message must survive the output limit. Add nearby
		// context around it rather than filling the budget before reaching it.
		start, end = anchor, anchor+1
		size := len(all[anchor].Text)*6 + 512
		for start > 0 && anchor-start < 4 {
			cost := len(all[start-1].Text)*6 + 512
			if size+cost > budget {
				break
			}
			size += cost
			start--
		}
		for end < len(all) && end-anchor < 16 {
			cost := len(all[end].Text)*6 + 512
			if size+cost > budget {
				break
			}
			size += cost
			end++
		}
	} else if after != "" {
		size := 0
		j := start
		for j < end && j-start < 200 {
			cost := len(all[j].Text)*6 + 512
			if j > start && size+cost > budget {
				break
			}
			size += cost
			j++
		}
		end = j
	} else {
		size := 0
		i := end
		for i > start && end-i < 200 {
			cost := len(all[i-1].Text)*6 + 512
			if i < end && size+cost > budget {
				break
			}
			size += cost
			i--
		}
		start = i
	}
	window := append([]appHistoryMessage{}, all[start:end]...)
	truncated := false
	for i := range window {
		truncated = truncated || window[i].Truncated
	}
	prev, next := "", ""
	if start > 0 && len(window) > 0 {
		prev = window[0].Key
	}
	if end < len(all) && len(window) > 0 {
		next = window[len(window)-1].Key
	}
	data := map[string]any{"provider": s.Provider, "conversation_id": s.ID, "name": appHistoryLabel(s.Label()), "cwd": s.Cwd, "messages": window, "cursor": map[string]string{"before": prev, "after": next}}
	return data, truncated || start > 0 || end < len(all), nil
}
func (d appHistoryDeps) askContext(ctx context.Context, opts map[string]string) (appHistoryOutput, error) {
	id, err := strconv.ParseInt(opts["id"], 10, 64)
	if err != nil || id <= 0 || len(opts["revision"]) != 64 || strings.Trim(opts["revision"], "0123456789abcdef") != "" || d.readAsk == nil {
		return appHistoryOutput{}, appHistoryFailure("invalid_request")
	}
	ask, err := d.readAsk(ctx, id, opts["revision"])
	if err != nil {
		return appHistoryOutput{}, err
	}
	data := map[string]any{"id": strconv.FormatInt(id, 10), "revision": state.AskActionRevision(ask), "provider": ask.Provider, "conversation_id": ask.SessionID, "first": appHistoryQuote(ask.FirstQuote), "latest": appHistoryQuote(ask.LastQuote), "placement": map[string]any{"confidence": "none", "anchor_ms": 0}, "messages": []appHistoryMessage{}, "cursor": map[string]string{"before": "", "after": ""}}
	s, err := d.session(ctx, ask.Provider, ask.SessionID)
	if err != nil {
		return appHistoryOutput{}, err
	}
	at, err := navPlaceAsk(ctx, state.AskPlacement{Provider: ask.Provider, SessionID: ask.SessionID, RaisedAt: ask.RaisedAt, FirstQuote: ask.FirstQuote, LatestQuote: ask.LastQuote}, s)
	if err != nil {
		return appHistoryOutput{}, appHistoryFailure("unavailable")
	}
	if at == nil {
		return appHistoryOutput{Data: data, Omitted: map[string]int{}}, nil
	}
	all, err := appHistoryMessagesWithQuote(ctx, s, []string{ask.LastQuote, ask.FirstQuote})
	if err != nil {
		return appHistoryOutput{}, err
	}
	turn := -1
	for i, t := range s.Turns {
		if appHistoryMillis(t.Started) == *at {
			turn = i
			break
		}
	}
	anchor := -1
	matched := false
	// A turn can contain hundreds of messages. Centre the peek on the actual
	// question rather than on the human prompt at the beginning of that turn.
	for _, q := range []string{ask.LastQuote, ask.FirstQuote} {
		if len(strings.TrimSpace(q)) < 16 {
			continue
		}
		for i, m := range all {
			if m.Turn == turn && m.Role == "assistant" && strings.Contains(m.Text, q) {
				anchor = i
				matched = true
			}
		}
		if matched {
			break
		}
	}
	if anchor < 0 {
		for i, m := range all {
			if m.Turn == turn && m.Role == "assistant" {
				anchor = i
				break
			}
		}
	}
	if anchor < 0 {
		return appHistoryOutput{Data: data, Omitted: map[string]int{}}, nil
	}
	all[anchor].Anchor = true
	window, partial, err := appHistorySlice(s, all, anchor, "", "")
	if err != nil {
		return appHistoryOutput{}, err
	}
	messages := window["messages"].([]appHistoryMessage)
	confidence := "time+text"
	if !matched {
		confidence = "time"
	}
	anchorAt := all[anchor].At
	if anchorAt <= 0 {
		anchorAt = *at
	}
	data["placement"] = map[string]any{"confidence": confidence, "anchor_ms": anchorAt}
	data["messages"] = messages
	data["cursor"] = window["cursor"]
	return appHistoryOutput{Data: data, Partial: partial, Omitted: map[string]int{}}, nil
}
func appHistoryQuote(raw string) map[string]any {
	text, cut := appHistoryText(raw, 256<<10)
	return map[string]any{"text": text, "bytes": len(raw), "truncated": cut}
}
