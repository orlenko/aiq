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
//
// Claude's aliases follow the newest model of each family (Fable 5.1, Opus
// 5.5, Sonnet 5.5, Haiku 5.5 as of 2026-10-10). The others name versions,
// checked that day against `codex`'s model list, `agy models` and
// `copilot help config`; update them when a newer model of a class ships.
var Models = map[string][4]string{
	"claude": {"fable", "opus", "sonnet", "haiku"},
	"codex":  {"gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-terra", "gpt-6-luna"},
	// Antigravity's own Gemini Argon is its strongest choice, ahead of the
	// Claude models it resells; it has nothing of the frontier class. Sonnet
	// is a "3p" model.
	"agy": {"", "gemini-4-argon-high", "claude-sonnet-5-5-high", "gemini-3.8-flash-high"},
	// Copilot offers no model newer than these.
	"copilot": {"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
}

// Legacy maps models a table once named to their tier, so sessions started
// on them keep their class and are moved to the current model when they
// are restarted. An Antigravity entry names the model without its effort
// suffix when it comes in several levels.
var Legacy = map[string]map[string]int{
	"codex": {"gpt-5.6-sol": 1, "gpt-6-sol": 1, "gpt-5.6-luna": 3},
	"agy":   {"claude-opus-4-6-thinking": 2, "claude-opus-5-5": 1},
}

// DefaultEffort is the effort level (high) aiq asks for when it picks a
// model and nobody named a level.
const DefaultEffort = 3

// Scopes are the quota-scope names of the tiers, where they differ from the
// model name. Antigravity's Gemini models draw on the account's main
// windows (no scope); its Claude model draws on the "3p" windows.
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

// Of returns the tier of a provider's model, current or legacy, or -1.
func Of(provider, model string) int {
	if model == "" {
		return -1
	}
	for i, m := range Models[provider] {
		if m == model {
			return i
		}
	}
	if n, ok := Legacy[provider][model]; ok {
		return n
	}
	return -1
}

// Current returns the model that replaces a legacy one, or model itself.
// Antigravity's suffixed names go through longrun, which keeps their level.
func Current(provider, model string) string {
	if n, ok := Legacy[provider][model]; ok && Has(provider, n) {
		return Models[provider][n]
	}
	return model
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
