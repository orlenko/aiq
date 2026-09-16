// Package tier maps aiq's portable model tiers and effort levels to each
// provider's own names, so a choice made for one CLI can be restated for
// the other.
package tier

// Models lists each provider's model for tiers 0 (strongest) to 3.
var Models = map[string][4]string{
	"claude": {"fable", "opus", "sonnet", "haiku"},
	"codex":  {"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"},
}

// Scopes are the quota-scope names of the tiers, where they differ from the
// model name.
var Scopes = map[string][4]string{
	"claude": Models["claude"],
	"codex":  {"astra", "sol", "terra", "luna"},
}

// Efforts lists effort levels 1 to 6 as each provider spells them.
var Efforts = map[string][6]string{
	"claude": {"low", "medium", "high", "xhigh", "max", "ultracode"},
	"codex":  {"low", "medium", "high", "xhigh", "max", "ultra"},
}

// Of returns the tier of a provider's model, or -1.
func Of(provider, model string) int {
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
