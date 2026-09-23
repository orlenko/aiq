package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tier"
)

// modelFlags translates shared controls only after a provider is chosen.
func modelFlags(provider string, f runFlags) (runFlags, error) {
	if !tier.Known(provider) {
		return f, fmt.Errorf("no model tiers for provider %q", provider)
	}
	var prefix []string
	model, level := "", ""
	if f.modelTier != nil {
		if f.modelScope != "" {
			return f, fmt.Errorf("--model-tier determines quota scope; omit --model-scope")
		}
		if !tier.Has(provider, *f.modelTier) {
			return f, fmt.Errorf("%s has no tier-%d model", provider, *f.modelTier)
		}
		model = tier.Models[provider][*f.modelTier]
		// Explicitly override the user's default scoped cap for this model.
		f.modelScope = tier.Scopes[provider][*f.modelTier]
	}
	if f.effort > 0 {
		level = tier.Efforts[provider][f.effort-1]
	}
	prefix = append(prefix, longrun.ModelArgs(provider, model, level)...)
	// A second model/effort could make quota selection disagree with execution.
	for i, arg := range f.rest {
		if arg == "--" {
			break
		}
		if f.modelTier != nil && (arg == "--model" || arg == "-m" || strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "-m=")) {
			return f, fmt.Errorf("use either --model-tier or a native model option")
		}
		if f.effort > 0 && (arg == "--effort" || strings.HasPrefix(arg, "--effort=")) {
			return f, fmt.Errorf("use either numeric --effort or a native effort option")
		}
		if provider == "codex" {
			var value string
			switch {
			case (arg == "-c" || arg == "--config") && i+1 < len(f.rest):
				value = f.rest[i+1]
			case strings.HasPrefix(arg, "--config="):
				value = strings.TrimPrefix(arg, "--config=")
			case strings.HasPrefix(arg, "-c") && len(arg) > 2:
				value = strings.TrimPrefix(arg[2:], "=")
			}
			key, _, _ := strings.Cut(value, "=")
			if (f.modelTier != nil && strings.TrimSpace(key) == "model") || (f.effort > 0 && strings.TrimSpace(key) == "model_reasoning_effort") {
				return f, fmt.Errorf("native config conflicts with --model-tier or --effort")
			}
		}
	}
	if provider == "codex" && len(f.rest) > 0 && (f.rest[0] == "exec" || f.rest[0] == "e") {
		f.rest = append(append([]string{f.rest[0]}, prefix...), f.rest[1:]...)
	} else {
		f.rest = append(prefix, f.rest...)
	}
	return f, nil
}

type autoRequest struct {
	print     bool
	prompt    string
	hasPrompt bool
}

func parseAutoFlags(args []string) (runFlags, autoRequest, error) {
	var r autoRequest
	var options []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-p" || arg == "--print":
			if r.hasPrompt || i+1 == len(args) {
				return runFlags{}, r, fmt.Errorf("auto -p requires one prompt")
			}
			i++
			r.print, r.hasPrompt, r.prompt = true, true, args[i]
		case arg == "--yolo":
			// Accepted for compatibility; auto always enables permission bypass.
		case arg == "--":
			if r.hasPrompt || len(args[i+1:]) != 1 {
				return runFlags{}, r, fmt.Errorf("auto accepts one literal prompt after --")
			}
			r.hasPrompt, r.prompt = true, args[i+1]
			i = len(args)
		default:
			options = append(options, arg)
			// Preserve option values, even if they resemble a portable flag.
			switch arg {
			case "--model-tier", "--effort", "--mode", "--wait", "--account", "--launcher", "--model-scope":
				if i+1 < len(args) {
					i++
					options = append(options, args[i])
				}
			}
		}
	}
	f := parseRunFlags(options)
	if f.flagsErr != nil {
		return f, r, f.flagsErr
	}
	if len(f.rest) > 0 {
		return f, r, fmt.Errorf("unsupported auto argument %q; use -p for a task, or select a provider for native options", f.rest[0])
	}
	if f.account != "" || f.launcher != "" || f.next || f.takeover != 0 || f.fallback != "" || f.resumeSession != "" || f.nudge != "" || f.modelScope != "" {
		return f, r, fmt.Errorf("auto supports --model-tier, --effort, --mode, --wait, --inherit-auth-env, -p, and --yolo; account, launcher, and session controls require a provider")
	}
	// A takeover replays the launch arguments, so a first prompt would be
	// sent again to every successor.
	if f.long && r.hasPrompt {
		return f, r, fmt.Errorf("a long auto session takes no prompt; type it in the session")
	}
	if os.Getenv("AIQ_BYPASS") == "1" {
		return f, r, fmt.Errorf("auto requires routing; unset AIQ_BYPASS or select a provider")
	}
	if f.modelTier == nil {
		tier := 1
		f.modelTier = &tier
	}
	return f, r, nil
}

// args is the CLI command line auto composes: the worker verb when -p was
// given, the permission bypass, and the prompt. Claude and Codex take the
// prompt after --; Antigravity and Copilot take it as the value of -p, and
// Copilot takes an interactive session's first prompt as the value of -i.
func (r autoRequest) args(provider string) []string {
	var args []string
	if provider == "copilot" && r.hasPrompt && !r.print {
		return []string{bypassFlag(provider), "-i", r.prompt}
	}
	if r.print {
		if provider == "agy" || provider == "copilot" {
			return append([]string{bypassFlag(provider)}, workerArgs(provider, r.prompt)...)
		}
		args = append(args, workerArgs(provider, "")[:1]...)
	}
	args = append(args, bypassFlag(provider))
	if r.hasPrompt {
		args = append(args, "--", r.prompt)
	}
	return args
}

// Rank each provider with its model scope, then compare their quota scores.
// Auto intentionally has no provider affinity: it spends the most perishable
// eligible quota across every pool, keeping the requested tier on every retry.
func rankAuto(policies map[string]selector.Policy, candidates []selector.Candidate) []selector.Ranked {
	var ranked []selector.Ranked
	byID := map[string]selector.Candidate{}
	for provider, policy := range policies {
		var cands []selector.Candidate
		for _, c := range candidates {
			if strings.HasPrefix(c.ID, provider+"/") {
				cands = append(cands, c)
				byID[c.ID] = c
			}
		}
		ranked = append(ranked, selector.Rank(policy, cands)...)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		ca, cb := byID[a.ID], byID[b.ID]
		provider, _, _ := strings.Cut(a.ID, "/")
		if policies[provider].Mode == state.ModeWorker {
			guarded := func(c selector.Candidate) bool { return c.InteractiveLeases > 0 && c.ResetCredits == 0 }
			if guarded(ca) != guarded(cb) {
				return !guarded(ca)
			}
		}
		if a.ResetCreditReady != b.ResetCreditReady {
			return a.ResetCreditReady
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if ca.Priority != cb.Priority {
			return ca.Priority < cb.Priority
		}
		if ca.LastSelectedAt != cb.LastSelectedAt {
			return ca.LastSelectedAt < cb.LastSelectedAt
		}
		return a.ID < b.ID
	})
	return ranked
}

func (a *app) selectAutoAccount(mode string, f runFlags, tried map[string]bool) (state.Account, []string, error) {
	now := time.Now()
	policies := map[string]selector.Policy{}
	var candidates []selector.Candidate
	var notes []string
	available, registered := 0, 0
	for _, provider := range config.Providers {
		if _, err := a.binary(provider); err != nil {
			notes = append(notes, "Skipping "+provider+": "+err.Error())
			continue
		}
		if !tier.Has(provider, *f.modelTier) {
			notes = append(notes, fmt.Sprintf("Skipping %s: no tier-%d model", provider, *f.modelTier))
			continue
		}
		available++
		cands, err := a.pool.Candidates(provider)
		if err != nil {
			return state.Account{}, notes, err
		}
		registered += len(cands)
		for _, c := range cands {
			if !tried[c.ID] {
				candidates = append(candidates, c)
			}
		}
		mf := f
		mf.modelScope = "" // the selected tier, not AIQ_MODEL_SCOPE, binds auto
		mf, err = modelFlags(provider, mf)
		if err != nil {
			return state.Account{}, notes, err
		}
		policyMode := mode
		if policyMode == state.ModeLong {
			policyMode = state.ModeInteractive
		}
		policy := a.pool.Policy(provider, policyMode, now)
		policy.ModelScope = mf.modelScope
		policies[provider] = policy
	}
	if available == 0 {
		return state.Account{}, notes, configErr("no-binary", "none of %s is available", providerList())
	}
	if registered == 0 {
		return state.Account{}, notes, configErr("no-accounts", "no accounts registered for available providers")
	}
	token := "pool-exhausted"
	for _, r := range rankAuto(policies, candidates) {
		if r.Eligible {
			acc, err := a.st.GetAccount(r.ID)
			if err != nil {
				return state.Account{}, notes, err
			}
			a.st.TouchSelected(acc.ID, now.Unix())
			return acc, notes, nil
		}
		if strings.HasPrefix(r.Reason, "at worker cap") {
			token = "at-worker-cap"
		}
		notes = append(notes, fmt.Sprintf("Skipping %s — %s", r.ID, r.Reason))
	}
	return state.Account{}, notes, poolDry(token, "no eligible account on any provider for model tier %d", *f.modelTier)
}
