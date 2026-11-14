package main

import "context"

// Match only the fixed controller_data.js detail schema, including all nested
// class arrays and raw/copy text. JSON bytes and UTF-16 code units are different
// admission domains. This walk neither edits nor truncates a cached envelope.
func navDetailAdmitStructure(ctx context.Context, e *navDetailEnvelope, entries, units int64) error {
	if ctx == nil || e == nil {
		return errNavDetailConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if entries == 0 && units == 0 {
		return nil
	}
	if entries <= 0 || units <= 0 {
		return errNavDetailConfiguration
	}
	array := func(n int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if int64(n) > entries {
			return errNavConversationTooLarge
		}
		entries -= int64(n)
		return nil
	}
	text := func(values ...string) error {
		walked := 0
		for _, value := range values {
			for _, r := range value {
				if walked%1024 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				walked++
				n := int64(1)
				if r > 0xffff {
					n = 2
				}
				// Invalid UTF8 is encoded as one replacement rune per invalid
				// byte by encoding/json, matching Go range and the browser.
				if n > units {
					return errNavConversationTooLarge
				}
				units -= n
			}
		}
		return ctx.Err()
	}
	if err := text(e.Build, e.Selection, e.TailKey, e.Session.Provider, e.Session.ID, e.Session.Cwd, e.Session.Title, e.Session.Model, e.Session.Branch); err != nil {
		return err
	}
	if err := array(len(e.Messages)); err != nil {
		return err
	}
	for _, m := range e.Messages {
		if err := text(m.ID, m.Key, m.Kind, m.Sub, m.Source, m.Account, m.Raw); err != nil {
			return err
		}
		if err := array(len(m.Groups)); err != nil {
			return err
		}
		for _, g := range m.Groups {
			if err := text(g.Head.Mode, g.Head.Who, g.Head.ClockMode, g.Head.ClockText); err != nil {
				return err
			}
			if err := array(len(g.Base)); err != nil {
				return err
			}
			if err := text(g.Base...); err != nil {
				return err
			}
			if err := array(len(g.Lines)); err != nil {
				return err
			}
			for _, line := range g.Lines {
				if err := text(line.Kind); err != nil {
					return err
				}
				if err := array(len(line.Spans)); err != nil {
					return err
				}
				for _, span := range line.Spans {
					if err := array(len(span.Classes)); err != nil {
						return err
					}
					if err := text(span.Classes...); err != nil {
						return err
					}
					if err := text(span.Text); err != nil {
						return err
					}
				}
			}
		}
	}
	return ctx.Err()
}
