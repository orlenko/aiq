// Package tier maps aiq's portable model tiers and effort levels to each
// provider's own names, so a choice made for one CLI can be restated for
// another.
package tier

// Models lists each provider's model for tiers 0 (strongest) to 3. A tier is
// a class of model, the same on every provider: 0 is the frontier (Fable,
// GPT-6 Astra), 1 the next step down (Opus, Sol), 2 the mid-size models
// (Sonnet, Terra), 3 the small fast ones (Haiku, Luna). An empty name means
// the provider has no model of that class; aiq then skips the provider at
// that tier rather than run an older model in its place.
var Models = map[string][4]string{
	"claude": {"fable", "opus", "sonnet", "haiku"},
	"codex":  {"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
	// Antigravity's newest models are a generation behind tiers 0 and 1.
	"agy":     {"", "", "claude-opus-4-6-thinking", "gemini-3.8-flash-medium"},
	"copilot": {"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
}

// Scopes are the quota-scope names of the tiers, where they differ from the
// model name. Antigravity's Gemini models draw on the account's main
// windows (no scope); its Claude models draw on the "3p" windows.
var Scopes = map[string][4]string{
	"claude":  Models["claude"],
	"codex":   {"astra", "sol", "terra", "luna"},
	"agy":     {"", "", "3p", ""},
	"copilot": Models["copilot"],
}

// Efforts lists effort levels 1 to 6 as each provider spells them. The
// Antigravity CLI knows three; the upper levels all map to its highest.
var Efforts = map[string][6]string{
	"claude":  {"low", "medium", "high", "xhigh", "max", "ultracode"},
	"codex":   {"low", "medium", "high", "xhigh", "max", "ultra"},
	"agy":     {"low", "medium", "high", "high", "high", "high"},
	"copilot": {"low", "medium", "high", "xhigh", "max", "max"},
}

// Has reports whether the provider has a model at tier n.
func Has(provider string, n int) bool {
	return n >= 0 && n < 4 && Models[provider][n] != ""
}

// Of returns the tier of a provider's model, or -1.
func Of(provider, model string) int {
	if model == "" {
		return -1
	}
	for i, m := range Models[provider] {
		if m == model {
			return i
		}
	}
	return -1
}

// EffortOf returns the 1-based effort level of a provider's spelling, or 0.
func EffortOf(provider, level string) int {
	for i, l := range Efforts[provider] {
		if l == level {
			return i + 1
		}
	}
	return 0
}

// Known reports whether the provider has a tier table.
func Known(provider string) bool {
	_, ok := Models[provider]
	return ok
}
