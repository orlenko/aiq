package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// ClaudeTitle reads the latest saved display name without parsing the
// conversation. A rename can occur long after the session's launch.
func ClaudeTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	title := ""
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"custom-title"`)) {
			var rec struct {
				Type  string `json:"type"`
				Title string `json:"customTitle"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "custom-title" {
				title = rec.Title
			}
		}
		if err != nil {
			if err != io.EOF {
				return ""
			}
			return title
		}
	}
}

// CodexTitle reads the last name recorded for id in the account's index.
// The index is shared with the real home by account overlays.
func CodexTitle(index, id string) string {
	return codexNames([]string{index})[id]
}
