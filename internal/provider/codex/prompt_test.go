package codex

import (
	"strings"
	"testing"
)

// Captured from codex 0.154.0 at 90% of the weekly window.
const rateLimitScreen = `› Reply with the single word OK. Do not run any tools.


⚠ Heads up, you have less than 10% of your weekly limit left. Run /status for a breakdown.

• OK


  Approaching rate limits
  Switch to gpt-5.6-luna for lower credit usage?

› 1. Switch to gpt-5.6-luna                 Fast and affordable agentic coding model.
  2. Keep current model
  3. Keep current model (never show again)  Hide future rate limit reminders about switching models.

  Press enter to confirm or esc to go back
`

func TestRateLimitPromptOpen(t *testing.T) {
	if !RateLimitPromptOpen(rateLimitScreen) {
		t.Error("menu with option 1 highlighted not detected")
	}
	moved := strings.Replace(strings.Replace(rateLimitScreen, "› 1.", "  1.", 1), "  2. Keep", "› 2. Keep", 1)
	if !RateLimitPromptOpen(moved) {
		t.Error("menu with option 2 highlighted not detected")
	}
	// The menu quoted in the transcript, with the composer below it.
	quoted := rateLimitScreen + strings.Repeat("• more output\n", 10) + "\n› Ask Codex to do anything\n  gpt-6-astra xhigh fast · main\n"
	if RateLimitPromptOpen(quoted) {
		t.Error("menu text scrolled above the composer matched")
	}
	if RateLimitPromptOpen("› Ask Codex to do anything\n") {
		t.Error("idle composer matched")
	}
}
