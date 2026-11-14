package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func navDetailOwnFacts(f navDetailFacts, count int) (navDetailFacts, error) {
	if f.stale && (f.live || f.hold) || !f.leaseKnown && f.leaseIdentity != "" {
		return navDetailFacts{}, errNavConversationInvalid
	}
	f.leaseIdentity = strings.Clone(f.leaseIdentity)
	if len(f.selectionContext) > 128 {
		return navDetailFacts{}, errNavConversationInvalid
	}
	f.selectionContext = strings.Clone(f.selectionContext)
	in := f.accounts
	f.accounts = make([]convoAccountFact, min(count, len(in)))
	for i := range f.accounts {
		f.accounts[i].known = in[i].known
		if in[i].known {
			f.accounts[i].label = strings.Clone(oneLine(in[i].label))
		}
	}
	return f, nil
}

// Parent priority is checked separately even with controlled context factories.
func navDetailStopped(parent, child context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	return child.Err()
}

type navDetailFrame struct {
	h             hash.Hash
	parent, child context.Context
}

func navDetailBool(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func (f navDetailFrame) add(fields ...string) error {
	var size [8]byte
	for _, field := range fields {
		if err := navDetailStopped(f.parent, f.child); err != nil {
			return err
		}
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = f.h.Write(size[:])
		// Bound temporary conversion storage even for a long supplied label.
		var chunk [4096]byte
		for len(field) > 0 {
			if err := navDetailStopped(f.parent, f.child); err != nil {
				return err
			}
			n := copy(chunk[:], field)
			_, _ = f.h.Write(chunk[:n])
			field = field[n:]
		}
	}
	return nil
}

func navDetailContextKey(parent, ctx context.Context, s *transcript.Session, snap transcript.Snapshot, q navDetailQuery, f navDetailFacts, o navConversationOptions, build string) (transcript.ViewKey, error) {
	start, end, err := navConversationWindowBounds(s.Turns, q.last, q.anchor, q.all)
	if err != nil {
		return transcript.ViewKey{}, err
	}
	frame := navDetailFrame{h: sha256.New(), parent: parent, child: ctx}
	if err := frame.add("aiq.nav.detail.v1", build, q.provider, q.id, strconv.FormatInt(o.bodyBytes, 10),
		strconv.FormatInt(q.anchor, 10), navDetailBool(f.live), o.skippedNote, o.liveMark,
		navDetailBool(f.leaseKnown), f.leaseIdentity); err != nil {
		return transcript.ViewKey{}, err
	}
	if o.arrayEntries != 0 || o.stringUnits != 0 {
		if err := frame.add("browser-structure-v1", strconv.FormatInt(o.arrayEntries, 10), strconv.FormatInt(o.stringUnits, 10)); err != nil {
			return transcript.ViewKey{}, err
		}
	}
	if o.selection != "" {
		if err := frame.add("selection-v1", o.selection); err != nil {
			return transcript.ViewKey{}, err
		}
	}
	if f.live && s.Unfinished {
		if err := frame.add("working-clock-v1", o.now.Format(time.RFC3339Nano)); err != nil {
			return transcript.ViewKey{}, err
		}
	}
	if q.all {
		if err := frame.add("all-prompts-v1"); err != nil {
			return transcript.ViewKey{}, err
		}
	}
	for i := start; i < end; i++ {
		account := convoAccountFact{}
		if i < len(f.accounts) {
			account = f.accounts[i]
		}
		if err := frame.add(strconv.Itoa(i), navDetailBool(account.known), account.label); err != nil {
			return transcript.ViewKey{}, err
		}
		turn := &s.Turns[i]
		// A conservative superset of potential header clocks. In particular,
		// relative labels can change six elapsed days after a prompt, mid-day.
		for _, at := range []struct{ relative, reply string }{
			{whenLabelIn(turn.Started, o.now, o.location), clockLabelIn(turn.Started, o.location)},
			{whenLabelIn(turn.ReplyAt, o.now, o.location), clockLabelIn(turn.ReplyAt, o.location)},
		} {
			if err := frame.add(at.relative, at.reply); err != nil {
				return transcript.ViewKey{}, err
			}
		}
		for _, item := range turn.Items {
			if err := frame.add(whenLabelIn(item.At, o.now, o.location), clockLabelIn(item.At, o.location)); err != nil {
				return transcript.ViewKey{}, err
			}
		}
	}
	if err := navDetailStopped(parent, ctx); err != nil {
		return transcript.ViewKey{}, err
	}
	return transcript.ViewKey{Snap: snap, Last: q.last, Markup: q.markup, Start: start,
		Lease: hex.EncodeToString(frame.h.Sum(nil)), Held: f.hold, Stale: f.stale}, nil
}
