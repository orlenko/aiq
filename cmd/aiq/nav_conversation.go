package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
)

var (
	errNavConversationInvalid  = errors.New("invalid conversation view request")
	errNavConversationAnchor   = errors.New("conversation anchor not found")
	errNavConversationTooLarge = errors.New("conversation view too large")
)

const navConversationSafeInteger int64 = 1<<53 - 1

type navConversationOptions struct {
	provider, id          string
	last                  int
	anchor                int64
	markup                bool
	now                   time.Time
	location              *time.Location
	live, hold, stale     bool
	accounts              []convoAccountFact
	skippedNote, liveMark string
	bodyBytes             int64
	faults                *diagnostic.Latch
}

type navConversationRender func(*transcript.Session, convoStyledOptions) ([]convoStyledMessage, error)

// This is a pure full-window producer, not a mounted route or cache wrapper.
// Callers own admission, facts and explicit display inputs. The returned DTO
// and exact encoded bytes must remain immutable if installed in a cache.
func buildNavConversation(s *transcript.Session, o navConversationOptions) (*navConversationWindow, []byte, string, error) {
	return buildNavConversationWith(s, o, renderConvoStyled, json.Marshal)
}

func buildNavConversationWith(s *transcript.Session, o navConversationOptions, render navConversationRender, marshal func(any) ([]byte, error)) (window *navConversationWindow, body []byte, fingerprint string, err error) {
	faults := o.faults
	if faults == nil {
		faults = &diagnostic.Latch{}
	}
	o.faults = faults
	defer func() {
		if value := recover(); value != nil {
			faults.Note(diagnostic.Capture(value))
			window, body, fingerprint, err = nil, nil, "", diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
		}
	}()
	if marshal == nil {
		return nil, nil, "", errNavConversationInvalid
	}
	window, err = projectNavConversationWith(s, o, render)
	if err != nil {
		return nil, nil, "", err
	}
	// Closed DTO compatibility is distinct from the caller's encoded limit.
	size, fits := navJSONSize(*window, math.MaxInt64)
	if !fits {
		return nil, nil, "", transcript.ErrUnshowable
	}
	if size > o.bodyBytes {
		return nil, nil, "", errNavConversationTooLarge
	}
	body, err = marshal(*window)
	if err != nil || int64(len(body)) != size {
		return nil, nil, "", transcript.ErrUnshowable
	}
	digest := sha256.Sum256(body)
	return window, body, hex.EncodeToString(digest[:]), nil
}

func projectNavConversation(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
	return projectNavConversationWith(s, o, renderConvoStyled)
}

// Projection is shared with the HTTP envelope, so that each consumer encodes
// its complete representation once. The established wire wrapper stays intact.
func projectNavConversationWith(s *transcript.Session, o navConversationOptions, render navConversationRender) (window *navConversationWindow, err error) {
	faults := o.faults
	if faults == nil {
		faults = &diagnostic.Latch{}
	}
	defer func() {
		if value := recover(); value != nil {
			faults.Note(diagnostic.Capture(value))
			window, err = nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
		}
	}()
	if s == nil || !transcript.IsFullID(o.provider, o.id) || s.Provider != o.provider || s.ID != o.id ||
		o.bodyBytes <= 0 || o.location == nil || o.skippedNote == "" || o.stale && (o.live || o.hold) || render == nil {
		return nil, errNavConversationInvalid
	}
	start, end, err := navConversationBounds(s.Turns, o.last, o.anchor)
	if err != nil {
		return nil, err
	}
	started, err := navConversationTime(s.Started)
	if err != nil {
		return nil, err
	}
	updated, err := navConversationTime(s.Updated)
	if err != nil {
		return nil, err
	}
	owned := *s
	if n := len(s.Turns); o.stale && n > 0 && s.Turns[n-1].Open {
		// Match loadConvo on the original tail. Only outer fields/slice headers
		// change here; the styled producer owns nested work before hold edits.
		owned.Turns = slices.Clone(s.Turns)
		last := &owned.Turns[n-1]
		last.Open = false
		if last.Reply == "" && len(last.Earlier) > 0 {
			last.Reply = last.Earlier[len(last.Earlier)-1]
			last.Earlier = last.Earlier[:len(last.Earlier)-1]
		}
	}
	owned.Turns = owned.Turns[:end]
	live, hold := o.live, o.hold
	if end < len(s.Turns) {
		live, hold = false, false
	}
	styled, err := render(&owned, convoStyledOptions{width: 100, markup: o.markup, now: o.now, location: o.location,
		liveMark: o.liveMark, skippedNote: o.skippedNote, start: start, live: live, hold: hold, accounts: o.accounts, faults: faults})
	if err != nil {
		faults.Note(diagnostic.FromError(err))
		return nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
	}
	messages, err := navConversationMessages(styled, o, start, end)
	if err != nil {
		return nil, err
	}
	window = &navConversationWindow{Session: navConversationSession{
		Provider: strings.Clone(s.Provider), ID: strings.Clone(s.ID), Cwd: strings.Clone(s.Cwd), Title: strings.Clone(s.Title),
		Model: strings.Clone(s.Model), Branch: strings.Clone(s.Branch), Started: started, Updated: updated, Worker: s.Worker, Bypass: s.Bypass,
	}, EarlierHidden: start, LaterHidden: len(s.Turns) - end, WindowStart: start, Reset: true, Markup: o.markup, Messages: messages}
	return window, nil
}

func navConversationBounds(turns []transcript.Turn, last int, anchor int64) (int, int, error) {
	if last < 1 || last > 10000 || anchor < 0 || anchor > 9999999999999 {
		return 0, 0, errNavConversationInvalid
	}
	if anchor == 0 {
		return convoStart(turns, last), len(turns), nil
	}
	for i, turn := range turns {
		at, err := navConversationTime(turn.Started)
		if err != nil {
			return 0, 0, err
		}
		if !at.Known || at.MS != anchor {
			continue
		}
		humans := 0
		for j := i; j < len(turns); j++ {
			if turns[j].Source == transcript.Human {
				humans++
				if humans > last {
					return i, j, nil
				}
			}
		}
		return i, len(turns), nil
	}
	return 0, 0, errNavConversationAnchor
}

func navConversationTime(t time.Time) (navWireTime, error) {
	if t.IsZero() {
		return navWireTime{}, nil
	}
	if t.Before(time.UnixMilli(-navConversationSafeInteger)) || !t.Before(time.UnixMilli(navConversationSafeInteger).Add(time.Millisecond)) {
		return navWireTime{}, transcript.ErrUnshowable
	}
	return navWireTime{MS: t.UnixMilli(), Known: true}, nil
}

func navConversationKey(fields ...string) string {
	h := sha256.New()
	var length [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func navConversationNumber(n int) bool { return n >= 0 && int64(n) <= navConversationSafeInteger }

func navConversationTurnKey(o navConversationOptions, start, turn int, rawKey string) string {
	return navConversationKey("aiq.nav.turn.v1", o.provider, o.id, strconv.Itoa(start), strconv.Itoa(turn), rawKey)
}
