package codex

import (
	"encoding/json"
	"fmt"
	"time"
)

// NewNamedThread creates a persisted, named thread without starting a turn.
// The interactive CLI then resumes it with the takeover prompt. Naming it
// before launch avoids a race with the first turn's automatic title.
func (p *Provider) NewNamedThread(home string, native bool, workspace, name string) (string, error) {
	s, err := p.open(home, native, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer s.close()
	raw, err := s.call("thread/start", map[string]any{"cwd": workspace})
	if err != nil {
		return "", s.fail(err)
	}
	var result struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("thread/start: %w", err)
	}
	if result.Thread.ID == "" {
		return "", fmt.Errorf("thread/start returned no thread id")
	}
	if _, err := s.call("thread/name/set", map[string]any{"threadId": result.Thread.ID, "name": name}); err != nil {
		return "", s.fail(err)
	}
	return result.Thread.ID, nil
}
