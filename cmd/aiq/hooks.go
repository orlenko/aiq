package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
)

// Hooks attached to long sessions. They must never break the session:
// every path exits 0, and a hook that has nothing to say prints nothing.
//
//	aiq claude-hook sessionstart|userpromptsubmit|stop
//	aiq codex-hook  sessionstart|userpromptsubmit|stop
//	aiq agy-hook    userpromptsubmit|stop
//
// Every CLI pipes a JSON payload on stdin. Claude and Codex accept the same
// output shapes: {"decision":"block","reason":…} on Stop makes the agent
// continue with the reason as its instruction; hookSpecificOutput.
// additionalContext on UserPromptSubmit injects context alongside the
// user's prompt. Antigravity's hooks are global (its CLI has no per-launch
// hook flag), fire on every model invocation rather than on the prompt, use
// camelCase fields, and take {"decision":"continue","reason":…} on Stop and
// {"injectSteps":[{"ephemeralMessage":…}]} before an invocation.
func cmdHook(provider string, args []string) {
	defer func() { recover() }()
	if len(args) == 0 {
		return
	}
	event := args[0]
	leaseID, _ := strconv.ParseInt(os.Getenv("AIQ_LEASE"), 10, 64)
	if os.Getenv("AIQ_LONG") != "1" || leaseID == 0 {
		return
	}
	input, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	var payload struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		StopHookActive bool   `json:"stop_hook_active"`
		ConversationID string `json:"conversationId"`
		Transcript     string `json:"transcriptPath"`
	}
	json.Unmarshal(input, &payload)
	if payload.SessionID == "" {
		payload.SessionID = payload.ConversationID
	}
	if payload.TranscriptPath == "" {
		payload.TranscriptPath = payload.Transcript
	}

	st, err := state.Open(paths.StateDB())
	if err != nil {
		return
	}
	defer st.Close()
	l, err := st.GetLease(leaseID)
	if err != nil {
		return
	}
	if l.Provider != "" && l.Provider != provider {
		return // a hook of one CLI inherited a lease of another (nested launch)
	}
	now := time.Now()
	if payload.SessionID != "" && payload.SessionID != l.SessionID {
		st.SetLeaseSession(l.ID, payload.SessionID)
	}
	if payload.TranscriptPath != "" && payload.TranscriptPath != l.Transcript {
		st.SetLeaseTranscript(l.ID, payload.TranscriptPath)
	}

	switch event {
	case "sessionstart":
		return
	case "userpromptsubmit":
		// Antigravity fires this before every model call of a turn; the
		// first one starts the turn and the rest keep it going.
		if provider != "agy" || !l.InTurn() {
			st.MarkTurn(l.ID, true, now)
		}
		if l.Drain == state.DrainRequested {
			remaining, until := currentHeadroom(st, l)
			st.SetLeaseDrain(l.ID, state.DrainDraining, now)
			st.LogEvent(provider, l.AccountID, "long", fmt.Sprintf("lease %d: wrap-up injected at prompt", l.ID), now)
			instruction := longrun.DrainInstruction(l.Workspace, remaining, until)
			var out map[string]any
			if provider == "agy" {
				out = map[string]any{"injectSteps": []map[string]any{{"ephemeralMessage": instruction}}}
			} else {
				out = map[string]any{"hookSpecificOutput": map[string]any{
					"hookEventName":     "UserPromptSubmit",
					"additionalContext": instruction,
				}}
			}
			json.NewEncoder(os.Stdout).Encode(out)
		}
	case "stop":
		st.MarkTurn(l.ID, false, now)
		switch l.Drain {
		case state.DrainRequested:
			if payload.StopHookActive {
				// Already continuing because of a stop hook; do not stack.
				return
			}
			remaining, until := currentHeadroom(st, l)
			st.SetLeaseDrain(l.ID, state.DrainDraining, now)
			st.LogEvent(provider, l.AccountID, "long", fmt.Sprintf("lease %d: wrap-up injected at turn end", l.ID), now)
			decision := "block"
			if provider == "agy" {
				decision = "continue"
			}
			out := map[string]any{"decision": decision, "reason": longrun.DrainInstruction(l.Workspace, remaining, until)}
			json.NewEncoder(os.Stdout).Encode(out)
		case state.DrainDraining:
			st.SetLeaseDrain(l.ID, state.DrainReady, now)
			st.LogEvent(provider, l.AccountID, "long", fmt.Sprintf("lease %d: wrap-up finished, ready for takeover", l.ID), now)
		}
	}
}

// currentHeadroom reads the tightest binding window for the hook's message.
func currentHeadroom(st *state.Store, l state.Lease) (float64, string) {
	ws, _ := st.ListWindows(l.AccountID)
	remaining := 100.0
	until := ""
	now := time.Now().Unix()
	for _, w := range ws {
		if w.Scope != "" || w.UsedPct < 0 || (w.Kind != state.KindShort && w.Kind != state.KindWeekly) || (w.ResetsAt > 0 && w.ResetsAt <= now) {
			continue
		}
		if rem := 100 - w.UsedPct; rem < remaining {
			remaining = rem
			if w.ResetsAt > 0 {
				until = ", resets " + time.Unix(w.ResetsAt, 0).Local().Format("15:04")
			}
		}
	}
	return remaining, until
}
